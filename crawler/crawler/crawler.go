package crawler

import (
	"os"
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/italia/developers-italia-backend/crawler/httpclient"
	"github.com/italia/developers-italia-backend/crawler/ipa"
	"github.com/italia/developers-italia-backend/crawler/elastic"
	"github.com/italia/developers-italia-backend/crawler/jekyll"
	"github.com/italia/developers-italia-backend/crawler/metrics"
	es "github.com/olivere/elastic"
	publiccode "github.com/italia/publiccode-parser-go"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Crawler is a helper class representing a crawler.
type Crawler struct{
	// Sync mutex guard.
	mu sync.Mutex
	es *es.Client
	index string
	domains []Domain
	repositories chan Repository
	wg sync.WaitGroup
}

// Repository is a single code repository. FileRawURL contains the direct url to the raw file.
type Repository struct {
	Name        string
	Hostname    string
	FileRawURL  string
	GitCloneURL string
	GitBranch   string
	Domain      Domain
	Pa          PA
	Headers     map[string]string
	Metadata    []byte
}

// NewCrawler initializes a new Crawler object, updates the IPA list and connects to Elasticsearch.
func NewCrawler() *Crawler {
	var c Crawler
	var err error

	// Make sure the data directory exists or spit an error
	if stat, err := os.Stat(viper.GetString("CRAWLER_DATADIR")); err != nil || !stat.IsDir() {
		log.Fatalf("The configured data directory (%v) does not exist: %v", viper.GetString("CRAWLER_DATADIR"), err)
	}

	// Read and parse list of domains.
	c.domains, err = ReadAndParseDomains("domains.yml")
	if err != nil {
		log.Fatal(err)
	}

	// Update ipa to lastest data.
	err = ipa.UpdateFromIndicePA()
	if err != nil {
		log.Error(err)
	}

	log.Debug("Connecting to ElasticSearch...")
	c.es, err = elastic.ClientFactory(
		viper.GetString("ELASTIC_URL"),
		viper.GetString("ELASTIC_USER"),
		viper.GetString("ELASTIC_PWD"))
	if err != nil {
		log.Fatal(err)
	}
	log.Debug("Successfully connected to ElasticSearch")

	// Initialize ES index mapping
	c.index = viper.GetString("ELASTIC_PUBLICCODE_INDEX")
	err = elastic.IndexMapping(c.index, c.es)
	if err != nil {
		log.Fatal(err)
	}

	// Create ES index with mapping "administration-codiceIPA".
	err = elastic.AdministrationsMapping(viper.GetString("ELASTIC_PUBLISHERS_INDEX"), c.es)
	if err != nil {
		log.Fatal(err)
	}

	// Initiate a channel of repositories.
	c.repositories = make(chan Repository, 1000)

	// Register Prometheus metrics.
	metrics.RegisterPrometheusCounter("repository_processed", "Number of repository processed.", c.index)
	metrics.RegisterPrometheusCounter("repository_file_saved", "Number of file saved.", c.index)
	metrics.RegisterPrometheusCounter("repository_file_indexed", "Number of file indexed.", c.index)
	metrics.RegisterPrometheusCounter("repository_cloned", "Number of repository cloned", c.index)
	//metrics.RegisterPrometheusCounter("repository_file_saved_valid", "Number of valid file saved.", c.index)

	return &c
}

// CrawlRepo crawls a single repository.
func (c *Crawler) CrawlRepo(repoURL string) error {
	log.Infof("Processing repository: %s", repoURL)

	// Parse as url.URL.
	u, err := url.Parse(repoURL)
	if err != nil {
		return fmt.Errorf("Invalid URL: %v", err)
	}

	// Check if current host is in known in domains.yml hosts.
	domain, err := c.KnownHost(repoURL, u.Hostname())
	if err != nil {
		return err
	}

	// Process repository.
	err = c.ProcessSingleRepository(repoURL, domain)
	if err != nil {
		return err
	}

	return c.crawl()
}

// CrawlOrgs processes a list of publishers.
func (c *Crawler) CrawlOrgs(publishers []PA) error {
	// Count configured orgs
	orgCount := 0
	for _, pa := range publishers {
		orgCount += len(pa.Organizations)
	}
	log.Infof("%v organizations belonging to %v publishers are going to be scanned",
		orgCount, len(publishers))
	
	// Process every item in publishers.
	for _, pa := range publishers {
		c.wg.Add(1)
		go c.ProcessPA(pa)
	}

	return c.crawl()
}

func (c *Crawler) crawl() error {
	// Start the metrics server.
	go metrics.StartPrometheusMetricsServer()

	// WaitingLoop check and close the repositories channel
	go c.WaitingLoop()
	
	// Process the repositories in order to retrieve the file.
	// ProcessRepositories is blocking (wait until c.repositories is closed by WaitingLoop).
	c.ProcessRepositories()

	// ElasticFlush to flush all the operations on ES.
	err := elastic.Flush(c.index, c.es)
	if err != nil {
		log.Errorf("Error flushing ElasticSearch: %v", err)
	}

	// Update Elastic alias.
	err = elastic.AliasUpdate(viper.GetString("ELASTIC_PUBLISHERS_INDEX"), viper.GetString("ELASTIC_ALIAS"), c.es)
	if err != nil {
		return fmt.Errorf("Error updating Elastic Alias: %v", err)
	}
	err = elastic.AliasUpdate(c.index, viper.GetString("ELASTIC_ALIAS"), c.es)
	if err != nil {
		return fmt.Errorf("Error updating Elastic Alias: %v", err)
	}

	return nil
}

// ExportForJekyll exports YAML data files for the Jekyll website.
func (c *Crawler) ExportForJekyll() error {
	return jekyll.GenerateJekyllYML(c.es)
}

// ProcessPA delegates the work to single PA crawlers.
func (c *Crawler) ProcessPA(pa PA) {
	log.Infof("Start ProcessPA on '%s'", pa.ID)

	// range over organizations..
	for _, org := range pa.Organizations {
		// Parse as url.URL.
		u, err := url.Parse(org)
		if err != nil {
			log.Errorf("invalid host: %v", err)
		}

		// Check if host is in list of "famous" hosts.
		domain, err := c.KnownHost(org, u.Hostname())
		if err != nil {
			log.Error(err)
		}

		// Process the PA domain
		c.ProcessPADomain(org, domain, pa)
	}

	c.wg.Done()
	log.Infof("End ProcessPA on '%s'", pa.ID)
}

// ProcessPADomain starts from the org page and process all the next.
func (c *Crawler) ProcessPADomain(orgURL string, domain Domain, pa PA) {
	// generateAPIURL
	orgURL, err := domain.generateAPIURL(orgURL)
	if err != nil {
		log.Errorf("generateAPIURL error: %v", err)
	}
	// Process the pages until the end is reached.
	for {
		log.Debugf("processAndGetNextURL handler: %s", orgURL)
		nextURL, err := domain.processAndGetNextURL(orgURL, &c.wg, c.repositories, pa)
		if err != nil {
			log.Errorf("error reading %s repository list: %v. NextUrl: %v", orgURL, err, nextURL)
			nextURL = ""
		}

		// If end is reached or fails, nextUrl is empty.
		if nextURL == "" {
			log.Infof("Url: %s - is the last one.", orgURL)
			return
		}
		// Update url to nextURL.
		orgURL = nextURL
	}
}

// WaitingLoop waits until all the goroutines counter is zero and close the repositories channel.
func (c *Crawler) WaitingLoop() {
	c.wg.Wait()

	// Close repositories channel.
	log.Debugf("closing repositories chan: len=%d", len(c.repositories))
	close(c.repositories)
}

// ProcessSingleRepository process a single repository given his url and domain.
func (c *Crawler) ProcessSingleRepository(url string, domain Domain) error {
	return domain.processSingleRepo(url, c.repositories)
}

// generateRandomInt returns an integer between 0 and max parameter.
// "Max" must be less than math.MaxInt32
func generateRandomInt(max int) (int, error) {
	result, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	return int(result.Int64()), err
}

// ProcessRepositories process the repositories channel and check the availability of the file.
func (c *Crawler) ProcessRepositories() {
	for repository := range c.repositories {
		c.wg.Add(1)
		go c.CheckAvailability(repository)
	}
	c.wg.Wait()
}

// CheckAvailability looks for the FileRawURL and, if found, save it.
func (c *Crawler) CheckAvailability(repository Repository) {
	name := repository.Name
	hostname := repository.Hostname
	fileRawURL := repository.FileRawURL
	gitURL := repository.GitCloneURL
	gitBranch := repository.GitBranch
	domain := repository.Domain
	headers := repository.Headers
	metadata := repository.Metadata
	pa := repository.Pa

	// Hash based on unique git repo URL.
	hash := sha1.New()
	_, err := hash.Write([]byte(gitURL))
	if err != nil {
		log.Errorf("Error generating the repository hash: %+v", err)
		c.wg.Done()
		return
	}
	hashedRepoURL := fmt.Sprintf("%x", hash.Sum(nil))

	// Increment counter for the number of repositories processed.
	metrics.GetCounter("repository_processed", c.index).Inc()

	resp, err := httpclient.GetURL(fileRawURL, headers)
	log.Debugf("repository checkAvailability: %s", name)

	// If it's available and no error returned.
	if resp.Status.Code == http.StatusOK && err == nil {
		c.mu.Lock()
		// Validate file. If invalid, terminate the check.
		err = validateRemoteFile(resp.Body, fileRawURL, pa)
		c.mu.Unlock()

		if err != nil {
			log.Errorf("%s is an invalid publiccode.", fileRawURL)
			log.Errorf("Errors: %+v", err)
			logBadYamlToFile(fileRawURL)
			c.wg.Done()
			return
		}

		// Save Metadata.
		err = SaveToFile(domain, hostname, name, metadata, c.index+"_metadata")
		if err != nil {
			log.Errorf("error saving to file: %v", err)
		}

		// Save to file.
		err = SaveToFile(domain, hostname, name, resp.Body, c.index)
		if err != nil {
			log.Errorf("error saving to file: %v", err)
		}

		// Clone repository.
		err = CloneRepository(domain, hostname, name, gitURL, gitBranch, c.index)
		if err != nil {
			log.Errorf("error cloning repository %s: %v", gitURL, err)
		}

		// Calculate Repository activity index and vitality.
		days := 60 // to add in configs.
		activityIndex, vitality, err := CalculateRepoActivity(domain, hostname, name, days)
		if err != nil {
			log.Errorf("error calculating repository Activity to file: %v", err)
		}
		log.Infof("Activity Index for %s: %f", name, activityIndex)
		var vitalitySlice []int
		for i := 0; i < len(vitality); i++ {
			vitalitySlice = append(vitalitySlice, int(vitality[i]))
		}

		// Save to ES.
		err = c.SaveToES(fileRawURL, hashedRepoURL, activityIndex, vitalitySlice, resp.Body)
		if err != nil {
			log.Errorf("error saving to ElastcSearch: %v", err)
		}
	}

	// Defer waiting group close.
	c.wg.Done()
}

func validateRemoteFile(data []byte, fileRawURL string, pa PA) error {
	parser := publiccode.NewParser() 
	parser.RemoteBaseURL = strings.TrimRight(fileRawURL, viper.GetString("CRAWLED_FILENAME"))

	err := parser.Parse(data)
	if err != nil {
		log.Errorf("Error parsing publiccode.yml for %s.", fileRawURL)
		return err
	}

	if pa.CodiceIPA != "" && parser.PublicCode.It.Riuso.CodiceIPA != "" && pa.CodiceIPA != parser.PublicCode.It.Riuso.CodiceIPA {
		return errors.New("codiceIPA for: " + fileRawURL + " is " + parser.PublicCode.It.Riuso.CodiceIPA + ", which differs from the one assigned to the org in the whitelist: " + pa.CodiceIPA)
	}

	return err
}
