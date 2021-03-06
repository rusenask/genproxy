package main

import (
	log "github.com/Sirupsen/logrus"
	"github.com/codegangsta/negroni"
	"github.com/elazarl/goproxy"
	"github.com/meatballhat/negroni-logrus"

	"bufio"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
)

const DefaultPort = ":8500"

// orPanic - wrapper for logging errors
func orPanic(err error) {
	if err != nil {
		log.WithFields(log.Fields{
			"error": err.Error(),
		}).Panic("Got error.")
	}
}

func main() {
	// getting proxy configuration
	verbose := flag.Bool("v", false, "should every proxy request be logged to stdout")
	record := flag.Bool("record", false, "should proxy record")
	destination := flag.String("destination", ".", "destination URI to catch")
	flag.Parse()

	// getting settings
	initSettings()

	// overriding default settings
	AppConfig.recordState = *record

	// overriding destination
	AppConfig.destination = *destination

	// getting default database
	port := os.Getenv("ProxyPort")
	if port == "" {
		port = DefaultPort
	} else {
		port = fmt.Sprintf(":%s", port)
	}

	// Output to stderr instead of stdout, could also be a file.
	log.SetOutput(os.Stderr)
	log.SetFormatter(&log.TextFormatter{})

	redisPool := getRedisPool()
	defer redisPool.Close()

	cache := Cache{pool: redisPool,
		prefix: AppConfig.cachePrefix}

	// getting connections
	d := DBClient{
		cache: cache,
		http:  &http.Client{},
	}

	// creating proxy
	proxy := goproxy.NewProxyHttpServer()

	proxy.OnRequest(goproxy.ReqHostMatches(regexp.MustCompile("^.*$"))).
		HandleConnect(goproxy.AlwaysMitm)

	// enable curl -p for all hosts on port 80
	proxy.OnRequest(goproxy.ReqHostMatches(regexp.MustCompile("^.*:80$"))).
		HijackConnect(func(req *http.Request, client net.Conn, ctx *goproxy.ProxyCtx) {
		defer func() {
			if e := recover(); e != nil {
				ctx.Logf("error connecting to remote: %v", e)
				client.Write([]byte("HTTP/1.1 500 Cannot reach destination\r\n\r\n"))
			}
			client.Close()
		}()
		clientBuf := bufio.NewReadWriter(bufio.NewReader(client), bufio.NewWriter(client))
		remote, err := net.Dial("tcp", req.URL.Host)
		orPanic(err)
		remoteBuf := bufio.NewReadWriter(bufio.NewReader(remote), bufio.NewWriter(remote))
		for {
			req, err := http.ReadRequest(clientBuf.Reader)
			orPanic(err)
			orPanic(req.Write(remoteBuf))
			orPanic(remoteBuf.Flush())
			resp, err := http.ReadResponse(remoteBuf.Reader, req)

			orPanic(err)
			orPanic(resp.Write(clientBuf.Writer))
			orPanic(clientBuf.Flush())
		}
	})

	// just helper handler to know where request hits proxy or no
	proxy.OnRequest().DoFunc(
		func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
			log.WithFields(log.Fields{
				"destination": r.URL.Host,
			}).Info("Got request")

			return r, nil
		})

	// hijacking plain connections
	proxy.OnRequest(goproxy.ReqHostMatches(regexp.MustCompile(*destination))).DoFunc(
		func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {

			log.Info("connection found......")
			log.Info(fmt.Sprintf("Url path:  %s", r.URL.Path))

			return d.processRequest(r)
		})

	go d.startAdminInterface()

	proxy.Verbose = *verbose

	// proxy starting message
	log.WithFields(log.Fields{
		"RedisAddress": AppConfig.redisAddress,
		"Destination":  *destination,
		"ProxyPort":    port,
	}).Info("Proxy is starting...")

	log.Warn(http.ListenAndServe(port, proxy))
}

// processRequest - processes incoming requests and based on proxy state (record/playback)
// returns HTTP response.
func (d *DBClient) processRequest(req *http.Request) (*http.Request, *http.Response) {

	if AppConfig.recordState {
		log.Info("*** RECORD ***")
		newResponse, err := d.recordRequest(req)
		if err != nil {
			// something bad happened, passing through
			return req, nil
		} else {
			// discarding original requests and returns supplied response
			return req, newResponse
		}

	} else {
		log.Info("*** PLAYBACK ***")
		newResponse := d.getResponse(req)
		return req, newResponse
	}
}

func (d *DBClient) startAdminInterface() {
	// starting admin interface
	mux := getBoneRouter(*d)
	n := negroni.Classic()
	n.Use(negronilogrus.NewMiddleware())
	n.UseHandler(mux)

	// admin interface starting message
	log.WithFields(log.Fields{
		"RedisAddress": AppConfig.redisAddress,
		"AdminPort":    AppConfig.adminInterface,
	}).Info("Admin interface is starting...")

	n.Run(AppConfig.adminInterface)
}
