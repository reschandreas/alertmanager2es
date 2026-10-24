package main

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/elastic/elastic-transport-go/v8/elastictransport"
	"github.com/elastic/go-elasticsearch/v9"
	"github.com/jessevdk/go-flags"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"

	"github.com/webdevops/alertmanager2es/config"
)

const (
	Author = "webdevops.io"
)

var (
	argparser *flags.Parser
	opts      config.Opts

	// Git version information
	gitCommit = "<unknown>"
	gitTag    = "<unknown>"
)

func main() {
	initArgparser()

	log.Infof("starting alertmanager2es v%s (%s; %s; by %v)", gitTag, gitCommit, runtime.Version(), Author)
	log.Info(string(opts.GetJson()))

	log.Infof("init exporter")
	exporter := &AlertmanagerElasticsearchExporter{}
	exporter.Init()

	transport := http.DefaultTransport.(*http.Transport).Clone()

	transport.Proxy = http.ProxyFromEnvironment

	options := []elasticsearch.Option{
		elasticsearch.WithAddresses(opts.Elasticsearch.Addresses...),
		elasticsearch.WithBasicAuth(opts.Elasticsearch.Username, opts.Elasticsearch.Password),
		elasticsearch.WithAPIKey(opts.Elasticsearch.ApiKey),
		elasticsearch.WithTransportOptions(
			elastictransport.WithTransport(transport),
		),
	}

	exporter.ConnectElasticsearch(options, opts.Elasticsearch.Index)

	// daemon mode
	log.Infof("starting http server on %s", opts.ServerBind)
	startHttpServer(exporter)
}

// init argparser and parse/validate arguments
func initArgparser() {
	argparser = flags.NewParser(&opts, flags.Default)
	_, err := argparser.Parse()

	// check if there is an parse error
	if err != nil {
		if flagsErr, ok := err.(*flags.Error); ok && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		} else {
			fmt.Println()
			argparser.WriteHelp(os.Stdout)
			os.Exit(1)
		}
	}

	// verbose level
	if opts.Logger.Verbose {
		log.SetLevel(log.DebugLevel)
	}

	// debug level
	if opts.Logger.Debug {
		log.SetReportCaller(true)
		log.SetLevel(log.TraceLevel)
		log.SetFormatter(&log.TextFormatter{
			CallerPrettyfier: func(f *runtime.Frame) (string, string) {
				s := strings.Split(f.Function, ".")
				funcName := s[len(s)-1]
				return funcName, fmt.Sprintf("%s:%d", path.Base(f.File), f.Line)
			},
		})
	}

	// json log format
	if opts.Logger.LogJson {
		log.SetReportCaller(true)
		log.SetFormatter(&log.JSONFormatter{
			DisableTimestamp: true,
			CallerPrettyfier: func(f *runtime.Frame) (string, string) {
				s := strings.Split(f.Function, ".")
				funcName := s[len(s)-1]
				return funcName, fmt.Sprintf("%s:%d", path.Base(f.File), f.Line)
			},
		})
	}
}

// start and handle prometheus handler
func startHttpServer(exporter *AlertmanagerElasticsearchExporter) {
	// healthz
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprint(w, "Ok"); err != nil {
			log.Error(err)
		}
	})

	http.HandleFunc("/webhook", exporter.HttpHandler)
	http.Handle("/metrics", promhttp.Handler())
	server := http.Server{ReadHeaderTimeout: 3 * time.Second, Addr: opts.ServerBind}

	log.Fatal(server.ListenAndServe())
}
