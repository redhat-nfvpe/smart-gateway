package events

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"sync"

	"github.com/MakeNowJust/heredoc"
	"github.com/infrawatch/smart-gateway/internal/pkg/amqp10"
	"github.com/infrawatch/smart-gateway/internal/pkg/api"
	"github.com/infrawatch/smart-gateway/internal/pkg/cacheutil"
	"github.com/infrawatch/smart-gateway/internal/pkg/events/incoming"
	"github.com/infrawatch/smart-gateway/internal/pkg/saconfig"
	"github.com/infrawatch/smart-gateway/internal/pkg/saelastic"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	//EVENTSINDEXTYPE value is used for creating Elasticsearch indexes holding event data
	EVENTSINDEXTYPE = "event"
	//APIHOME value contains root API endpoint content
	APIHOME = `
<html>
	<head>
		<title>Smart Gateway Event API</title>
	</head>
	<body>
		<h1>API</h1>
		<ul>
			<li>/alerts POST alerts in JSON format on to AMQP message bus</li>
			<li>/metrics GET metric data</li>
		</ul>
	</body>
</html>
`
)

/*************** main routine ***********************/
// eventusage and command-line flags
func eventusage() {
	doc := heredoc.Doc(`
  For running with config file use
	********************* config *********************
	$go run cmd/main.go -config smartgateway_config.json -servicetype events
	**************************************************`)

	fmt.Fprintln(os.Stderr, `Required command line argument missing`)
	fmt.Fprintln(os.Stdout, doc)
	flag.PrintDefaults()
}

var debuge = func(format string, data ...interface{}) {} // Default no debugging output

//spawnAPIServer spawns goroutine which provides http API for alerts and metrics statistics for Prometheus
func spawnAPIServer(wg *sync.WaitGroup, finish chan bool, serverConfig saconfig.EventConfiguration, metricHandler *api.EventMetricHandler, amqpHandler *amqp10.AMQPHandler) {
	prometheus.MustRegister(metricHandler, amqpHandler)
	// Including these stats kills performance when Prometheus polls with multiple targets
	prometheus.Unregister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	prometheus.Unregister(prometheus.NewGoCollector())

	ctxt := api.NewContext(serverConfig)
	http.Handle("/alert", api.Handler{Context: ctxt, H: api.AlertHandler})
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(APIHOME))
	})
	srv := &http.Server{Addr: serverConfig.API.APIEndpointURL}
	// spawn shutdown signal handler
	go func() {
		//lint:ignore S1000 reason: we are waiting for channel close, value might not be ever received
		select {
		case <-finish:
			if err := srv.Shutdown(context.Background()); err != nil {
				log.Fatalf("Failed to stop API server: %s\n", err)
				// in case of error we need to allow wait group to end
				wg.Done()
			}
		}
	}()
	// spawn the API server
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Println("Started API server")
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("Failed to start API server: %s\n", err.Error())
		} else {
			log.Println("Closing API server")
		}
	}()
}

//notifyAlertManager generates alert from event for Prometheus Alert Manager
func notifyAlertManager(wg *sync.WaitGroup, serverConfig saconfig.EventConfiguration, event *incoming.EventDataFormat, record string) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		generatorURL := fmt.Sprintf("%s/%s/%s/%s", serverConfig.ElasticHostURL, (*event).GetIndexName(), EVENTSINDEXTYPE, record)
		alert, err := (*event).GeneratePrometheusAlertBody(generatorURL)
		if err != nil {
			log.Printf("Failed generate alert from event:\n- error: %s\n- event: %s\n", err, (*event).GetSanitized())
		}
		debuge("Debug: Generated alert:\n%s\n", alert)
		var byteAlertBody = []byte(fmt.Sprintf("[%s]", alert))
		req, _ := http.NewRequest("POST", serverConfig.AlertManagerURL, bytes.NewBuffer(byteAlertBody))
		req.Header.Set("X-Custom-Header", "smartgateway")
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Failed to report alert to AlertManager:\n- error: %s\n- alert: %s\n", err, alert)
			body, _ := ioutil.ReadAll(resp.Body)
			defer resp.Body.Close()
			debuge("Debug:response Status:%s\n", resp.Status)
			debuge("Debug:response Headers:%s\n", resp.Header)
			debuge("Debug:response Body:%s\n", string(body))
		}
		log.Println("Closing Alert Manager notifier")
	}()
}

//StartEvents is the entry point for running smart-gateway in events mode
func StartEvents() {
	var wg sync.WaitGroup
	finish := make(chan bool)

	amqp10.SpawnSignalHandler(finish, os.Interrupt)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// set flags for parsing options
	flag.Usage = eventusage
	fServiceType := flag.String("servicetype", "event", "Event type")
	fConfigLocation := flag.String("config", "", "Path to configuration file.")
	fUniqueName := flag.String("uname", "events-"+strconv.Itoa(rand.Intn(100)), "Unique name across application")
	flag.Parse()

	//load configuration from given config file or from cmdline parameters
	var serverConfig *saconfig.EventConfiguration
	if len(*fConfigLocation) > 0 {
		conf, err := saconfig.LoadConfiguration(*fConfigLocation, "event")
		if err != nil {
			log.Fatal("Config Parse Error: ", err)
		}
		serverConfig = conf.(*saconfig.EventConfiguration)
		serverConfig.ServiceType = *fServiceType
	} else {
		eventusage()
		os.Exit(1)
	}

	if serverConfig.Debug {
		debuge = func(format string, data ...interface{}) { log.Printf(format, data...) }
	}

	if len(serverConfig.AMQP1EventURL) == 0 && len(serverConfig.AMQP1Connections) == 0 {
		log.Println("Configuration option 'AMQP1EventURL' or 'AMQP1Connections' is required")
		eventusage()
		os.Exit(1)
	}

	if len(serverConfig.ElasticHostURL) == 0 {
		log.Println("Configuration option 'ElasticHostURL' is required")
		eventusage()
		os.Exit(1)
	} else {
		log.Printf("Elasticsearch configured at %s\n", serverConfig.ElasticHostURL)
	}

	if len(serverConfig.AlertManagerURL) > 0 {
		log.Printf("AlertManager configured at %s\n", serverConfig.AlertManagerURL)
		serverConfig.AlertManagerEnabled = true
	} else {
		log.Println("AlertManager disabled")
	}

	if len(serverConfig.API.APIEndpointURL) > 0 {
		debuge("API configured at %s\n", serverConfig.API.APIEndpointURL)
		serverConfig.APIEnabled = true
	} else {
		log.Println("API disabled")
	}

	if len(serverConfig.API.AMQP1PublishURL) > 0 {
		log.Printf("AMQP1.0 publish address configured at %s\n", serverConfig.API.AMQP1PublishURL)
		serverConfig.PublishEventEnabled = true
	} else {
		log.Println("AMQP1.0 publish address disabled")
	}

	if len(serverConfig.AMQP1EventURL) > 0 {
		//TO-DO(mmagr): Remove this in next major release
		serverConfig.AMQP1Connections = []saconfig.AMQPConnection{
			saconfig.AMQPConnection{
				URL:          serverConfig.AMQP1EventURL,
				DataSourceID: saconfig.DataSourceCollectd,
				DataSource:   "collectd",
			},
		}
	}
	for _, conn := range serverConfig.AMQP1Connections {
		log.Printf("AMQP1.0 %s listen address configured at %s\n", conn.DataSource, conn.URL)
	}

	applicationHealth := cacheutil.NewApplicationHealthCache()
	metricHandler := api.NewAppStateEventMetricHandler(applicationHealth)
	amqpHandler := amqp10.NewAMQPHandler("Event Consumer")

	// Elastic connection
	elasticClient, err := saelastic.CreateClient(*serverConfig)

	if err != nil {
		log.Fatal(err.Error())
	}
	log.Println("Connected to Elasticsearch")
	applicationHealth.ElasticSearchState = 1

	// API spawn
	if serverConfig.APIEnabled {
		spawnAPIServer(&wg, finish, *serverConfig, metricHandler, amqpHandler)
	}

	// AMQP connection(s)
	processingCases, qpidStatusCases, amqpServers := amqp10.CreateMessageLoopComponents(serverConfig, finish, amqpHandler, *fUniqueName)
	amqp10.SpawnQpidStatusReporter(&wg, applicationHealth, qpidStatusCases)

	// spawn handler manager
	handlerManager, err := NewEventHandlerManager(*serverConfig)
	if err != nil {
		log.Fatal(err.Error())
	}

	// spawn event processor
	wg.Add(1)
	go func() {
		defer wg.Done()
		finishCase := len(processingCases) - 1
	processingLoop:
		for {
			switch index, msg, _ := reflect.Select(processingCases); index {
			case finishCase:
				break processingLoop
			default:
				// NOTE: below will panic for generic data source until the appropriate logic will be implemented
				event := incoming.NewFromDataSource(amqpServers[index].DataSource)
				amqpServers[index].Server.GetHandler().IncTotalMsgProcessed()
				err := event.ParseEvent(msg.String())
				if err != nil {
					log.Printf("Failed to parse received event:\n- error: %s\n- event: %s\n", err, event)
				}

				process := true
				for _, handler := range handlerManager.Handlers[amqpServers[index].DataSource] {
					if handler.Relevant(event) {
						process, err = handler.Handle(event, elasticClient)
						if !process {
							if err != nil {
								log.Print(err.Error())
							}
							break
						}
					}
				}
				if process {
					record, err := elasticClient.Create(event.GetIndexName(), EVENTSINDEXTYPE, event.GetRawData())
					if err != nil {
						applicationHealth.ElasticSearchState = 0
						log.Printf("Failed to save event to Elasticsearch DB:\n- error: %s\n- event: %s\n", err, event)
					} else {
						applicationHealth.ElasticSearchState = 1
					}
					if serverConfig.AlertManagerEnabled {
						notifyAlertManager(&wg, *serverConfig, &event, record)
					}
				}
			}
		}
		log.Println("Closing event processor.")
	}()

	// do not end until all loop goroutines ends
	wg.Wait()
	log.Println("Exiting")
}
