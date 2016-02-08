package main

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	log "gopkg.in/inconshreveable/log15.v2"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Page struct {
	URL       *url.URL
	Processed bool
	Depth     uint16
	Links     []*Link
	Error     *error
}

type Link struct {
	URL      *url.URL
	External bool
	Depth    uint16
}

type Task struct {
	URL   *url.URL
	Depth uint16
}

var logger = log.New()

func main() {
	var maxDepth uint16
	var disallow []string
	var quiet bool
	var verbose bool
	var numWorkers uint16
	var numConns int
	var zeroBothers bool
	var delay float64

	cmd := &cobra.Command{
		Use:   "gergle URL",
		Short: "Website crawler.",
	}
	cmd.Flags().Uint16VarP(&maxDepth, "depth", "d", 100, "Maximum crawl depth.")
	cmd.Flags().StringSliceVarP(&disallow, "disallow", "i", nil, "Disallowed paths.")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "No logging to stderr.")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output logging.")
	cmd.Flags().Uint16VarP(&numWorkers, "workers", "w", 10, "Number of concurrent http-getting workers.")
	cmd.Flags().IntVarP(&numConns, "connections", "c", 5, "Maximum number of open connections to the server.")
	cmd.Flags().BoolVarP(&zeroBothers, "zero", "", false, "The number of bothers given about robots.txt. ")
	cmd.Flags().Float64VarP(&delay, "delay", "t", -1, "The number of seconds between requests to the server.")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		// Configure logging.
		var logLevel log.Lvl
		if verbose && quiet {
			return errors.New("--verbose and --quiet are mutually exclusive options.")
		} else if verbose {
			logLevel = log.LvlDebug
		} else if quiet {
			logLevel = log.LvlCrit
		} else {
			logLevel = log.LvlInfo
		}
		logger.SetHandler(log.LvlFilterHandler(logLevel, log.StderrHandler))

		// Ensure the user provides only a single URL.
		if len(args) < 1 {
			return errors.New("URL argument required.")
		} else if len(args) > 1 {
			return errors.New("Unexpected arguments after URL.")
		}

		// Ensure the user has provided a valid URL.
		initUrl, err := url.Parse(args[0])
		if err != nil || (initUrl.Scheme != "http" && initUrl.Scheme != "https") {
			return errors.New("Expected URL of the form http[s]://...")
		}

		client := &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost: numConns,
		}}

		if !zeroBothers {
			robots, err := fetchRobots(client, initUrl)
			if err == nil {
				disallow = append(disallow, readDisallowRules(robots)...)
				if delay < 0 {
					delay = readCrawlDelay(robots)
				}
			} else {
				logger.Info("Failed to fetch robots.txt", "error", err)
			}
		}

		delayDuration := time.Duration(0)
		if delay >= 0 {
			delayDuration = time.Duration(delay * 1e9)
		}

		// Crawling.
		pages := make(chan Page, 10)
		go crawl(client, initUrl, pages, maxDepth, parseDisallowRules(disallow), numWorkers, delayDuration)

		// Output.
		for page := range pages {
			fmt.Printf("URL: %s, Depth: %d, Links: %d\n", page.URL, page.Depth, len(page.Links))
		}

		return nil
	}

	cmd.Execute()
}

func fetchRobots(client *http.Client, u *url.URL) ([]byte, error) {
	robotsPath, _ := url.Parse("/robots.txt")
	robotsUrl := u.ResolveReference(robotsPath).String()
	logger.Info("Fetching robots.txt", "url", robotsUrl)
	resp, err := client.Get(robotsUrl)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, errors.New("robots.txt not found.")
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

var robotsTxtDisallowRules = regexp.MustCompile("(?is)Disallow:\\s*(.+?)(\\s|$)")

func readDisallowRules(body []byte) (rules []string) {
	n := bytes.IndexByte(body, 0)
	for _, rule := range robotsTxtDisallowRules.FindAllSubmatch(body, n) {
		rules = append(rules, string(rule[1]))
	}
	return
}

var crawlDelayRegex = regexp.MustCompile("(?si)\\s*Crawl-Delay:\\s*([\\d\\.]+)")

func readCrawlDelay(body []byte) float64 {
	delayMatch := crawlDelayRegex.FindSubmatch(body)
	if delayMatch == nil {
		return 0
	}
	delay, err := strconv.ParseFloat(string(delayMatch[1]), 64)
	if err != nil {
		return 0
	}
	return delay
}

func sanitizeURL(u *url.URL) string {
	us := u.String()

	// Remove the fragment
	f := strings.Index(us, "#")
	if f != -1 {
		us = us[:f]
	}

	// Remove trailing slashes
	return strings.TrimRight(us, "/")
}

func crawl(client *http.Client, initUrl *url.URL, out chan<- Page, maxDepth uint16, disallow []*regexp.Regexp, numWorkers uint16, delay time.Duration) {
	unexplored := sync.WaitGroup{}
	logger.Info("Starting crawl", "url", initUrl, "delay", delay, "maxDepth", maxDepth, "disallow", disallow, "workers", numWorkers)

	var ticker <-chan time.Time
	if delay > 0 {
		ticker = time.Tick(delay)
	}

	// Prepare the work queue.
	pending := make(chan Task, 100)
	pending <- Task{initUrl, 0}
	unexplored.Add(1)

	follow := func(link *Link) bool {
		if link.External {
			logger.Debug("Skipping external link", "url", link.URL)
			return false
		}
		if link.Depth > maxDepth {
			logger.Debug("Skipping deep link", "url", link.URL, "depth", link.Depth, "maxDepth", maxDepth)
			return false
		}

		for _, disallowRule := range disallow {
			if disallowRule.MatchString(link.URL.Path) {
				logger.Debug("Skipping disallowed link", "url", link.URL, "rule", disallowRule)
				return false
			}
		}

		return true
	}

	links := make(chan *Link, 100)
	go func(init ...*url.URL) {
		seen := make(map[string]bool)

		// Mark the URLs queued elsewhere as being seen.
		for _, initUrl := range init {
			seen[sanitizeURL(initUrl)] = true
		}

		for link := range links {
			if !follow(link) {
				// fmt.Printf("- Skipping external link: %#v\n", link)
				unexplored.Done()
				continue
			}

			sanUrl := sanitizeURL(link.URL)
			_, linkSeen := seen[sanUrl]
			if linkSeen {
				// fmt.Printf("- Skipping seen link: %#v\n", link)
				unexplored.Done()
				continue
			}

			// Queue the link.
			seen[sanUrl] = true
			pending <- LinkTask(link)
		}
	}(initUrl)

	// Request pending, and requeue discovered pages.
	for w := uint16(0); w < numWorkers; w++ {
		go func() {
			for task := range pending {
				if ticker != nil {
					<-ticker
				}
				page := process(client, task)
				out <- page

				for _, link := range page.Links {
					// fmt.Printf("  Found link %#v\n", link)
					unexplored.Add(1)
					links <- link
				}
				unexplored.Done()
			}
		}()
	}

	unexplored.Wait()
	close(links)
	close(out)
}

func LinkTask(link *Link) Task {
	return Task{URL: link.URL, Depth: link.Depth}
}

func ErrorPage(pageURL *url.URL, depth uint16, err error) Page {
	return Page{pageURL, false, depth, []*Link{}, &err}
}

func FollowLink(href string, base *url.URL, depth uint16) (*Link, error) {
	hrefUrl, err := url.Parse(href)
	if err != nil {
		return nil, err
	}

	abs := base.ResolveReference(hrefUrl)
	return &Link{
		URL:      abs,
		External: abs.Scheme != base.Scheme || abs.Host != base.Host,
		Depth:    depth,
	}, nil
}

func parseDisallowRule(rule string) *regexp.Regexp {
	return regexp.MustCompile("^/?" + strings.Replace(regexp.QuoteMeta(strings.TrimLeft(rule, "/")), "\\*", "*", -1))
}

func parseDisallowRules(rules []string) (regexpRules []*regexp.Regexp) {
	regexpRules = make([]*regexp.Regexp, 0)
	for _, rule := range rules {
		regexpRules = append(regexpRules, parseDisallowRule(rule))
	}
	return
}

func process(client *http.Client, task Task) Page {
	resp, err := client.Get(task.URL.String())
	if err != nil {
		return ErrorPage(task.URL, task.Depth, err)
	}

	defer resp.Body.Close()
	return parsePage(task.URL, task.Depth, resp)
}

func parsePage(pageUrl *url.URL, depth uint16, resp *http.Response) Page {
	if resp.StatusCode != 200 {
		logger.Debug("Not processing non-200 status code", "url", pageUrl, "status", resp.StatusCode)
		return ErrorPage(pageUrl, depth, errors.New("Non-200 response"))
	}

	mime := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(mime), "html") {
		logger.Debug("Doesn't look like HTML", "url", pageUrl, "content-type", mime)
		return ErrorPage(pageUrl, depth, errors.New("Doesn't look like HTML"))
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logger.Warn("Failed to read body", "url", pageUrl)
		return ErrorPage(pageUrl, depth, err)
	}

	base := parseBase(resp, body)
	return Page{pageUrl, true, depth, parseLinks(base, body, depth+1), nil}
}

var baseRegex = regexp.MustCompile("(?is)<base[^>]+href=[\"']?(.+?)['\"\\s>]")

func parseBase(resp *http.Response, body []byte) *url.URL {
	base := baseRegex.FindSubmatch(body)
	if base != nil {
		baseUrl, err := url.Parse(string(base[1]))
		if err == nil {
			// Use the <base href="..."> from the page body.
			return resp.Request.URL.ResolveReference(baseUrl)
		}
	}

	return resp.Request.URL
}

// Attribution: definitely not http://stackoverflow.com/a/1732454/123600.
var anchorRegex = regexp.MustCompile("(?is)<a[^>]+href=[\"']?(.+?)['\"\\s>]")

func parseLinks(base *url.URL, body []byte, depth uint16) (links []*Link) {
	n := bytes.IndexByte(body, 0)
	for _, anchor := range anchorRegex.FindAllSubmatch(body, n) {
		link, err := FollowLink(string(anchor[1]), base, depth)
		if err != nil {
			logger.Debug("Failed to parse href", "href", anchor[1])
			continue
		}
		links = append(links, link)
	}

	return
}
