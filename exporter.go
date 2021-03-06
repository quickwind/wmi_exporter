// +build windows

package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/StackExchange/wmi"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"github.com/quickwind/wmi_exporter/collector"
	"gopkg.in/alecthomas/kingpin.v2"
)

// WmiCollector implements the prometheus.Collector interface.
type WmiCollector struct {
	collectors map[string]collector.Collector
}

const (
	defaultCollectors            = "cpu,cs,logical_disk,net,os,service,system,textfile"
	defaultCollectorsPlaceholder = "[defaults]"
	serviceName                  = "wmi_exporter"
)

var (
	scrapeDurationDesc = prometheus.NewDesc(
		prometheus.BuildFQName(collector.Namespace, "exporter", "collector_duration_seconds"),
		"wmi_exporter: Duration of a collection.",
		[]string{"collector"},
		nil,
	)
	scrapeSuccessDesc = prometheus.NewDesc(
		prometheus.BuildFQName(collector.Namespace, "exporter", "collector_success"),
		"wmi_exporter: Whether the collector was successful.",
		[]string{"collector"},
		nil,
	)

	// This can be removed when client_golang exposes this on Windows
	// (See https://github.com/prometheus/client_golang/issues/376)
	startTime     = float64(time.Now().Unix())
	startTimeDesc = prometheus.NewDesc(
		"process_start_time_seconds",
		"Start time of the process since unix epoch in seconds.",
		nil,
		nil,
	)
)

// Describe sends all the descriptors of the collectors included to
// the provided channel.
func (coll WmiCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- scrapeDurationDesc
	ch <- scrapeSuccessDesc
}

// Collect sends the collected metrics from each of the collectors to
// prometheus. Collect could be called several times concurrently
// and thus its run is protected by a single mutex.
func (coll WmiCollector) Collect(ch chan<- prometheus.Metric) {
	wg := sync.WaitGroup{}
	wg.Add(len(coll.collectors))
	for name, c := range coll.collectors {
		go func(name string, c collector.Collector) {
			execute(name, c, ch)
			wg.Done()
		}(name, c)
	}

	ch <- prometheus.MustNewConstMetric(
		startTimeDesc,
		prometheus.CounterValue,
		startTime,
	)
	wg.Wait()
}

func filterAvailableCollectors(collectors string) string {
	var availableCollectors []string
	for _, c := range strings.Split(collectors, ",") {
		_, ok := collector.Factories[c]
		if ok {
			availableCollectors = append(availableCollectors, c)
		}
	}
	return strings.Join(availableCollectors, ",")
}

func execute(name string, c collector.Collector, ch chan<- prometheus.Metric) {
	begin := time.Now()
	err := c.Collect(ch)
	duration := time.Since(begin)
	var success float64

	if err != nil {
		log.Errorf("collector %s failed after %fs: %s", name, duration.Seconds(), err)
		success = 0
	} else {
		log.Debugf("collector %s succeeded after %fs.", name, duration.Seconds())
		success = 1
	}
	ch <- prometheus.MustNewConstMetric(
		scrapeDurationDesc,
		prometheus.GaugeValue,
		duration.Seconds(),
		name,
	)
	ch <- prometheus.MustNewConstMetric(
		scrapeSuccessDesc,
		prometheus.GaugeValue,
		success,
		name,
	)
}

func expandEnabledCollectors(enabled string) []string {
	expanded := strings.Replace(enabled, defaultCollectorsPlaceholder, defaultCollectors, -1)
	separated := strings.Split(expanded, ",")
	unique := map[string]bool{}
	for _, s := range separated {
		if s != "" {
			unique[s] = true
		}
	}
	result := make([]string, 0, len(unique))
	for s := range unique {
		result = append(result, s)
	}
	return result
}

func loadCollectors(list string) (map[string]collector.Collector, error) {
	collectors := map[string]collector.Collector{}
	enabled := expandEnabledCollectors(list)

	for _, name := range enabled {
		fn, ok := collector.Factories[name]
		if !ok {
			return nil, fmt.Errorf("collector '%s' not available", name)
		}
		c, err := fn()
		if err != nil {
			return nil, err
		}
		collectors[name] = c
	}
	return collectors, nil
}

func init() {
	prometheus.MustRegister(version.NewCollector("wmi_exporter"))
}

func initWbem() {
	// This initialization prevents a memory leak on WMF 5+. See
	// https://github.com/martinlindhe/wmi_exporter/issues/77 and linked issues
	// for details.
	log.Debugf("Initializing SWbemServices")
	s, err := wmi.InitializeSWbemServices(wmi.DefaultClient)
	if err != nil {
		log.Fatal(err)
	}
	wmi.DefaultClient.AllowMissingFields = true
	wmi.DefaultClient.SWbemServicesClient = s
}

func main() {
	var (
		metricsOutput = kingpin.Flag(
			"metrics.output",
			"Output file for the metrics.",
		).Default("./metrics.out").String()
		enabledCollectors = kingpin.Flag(
			"collectors.enabled",
			"Comma-separated list of collectors to use. Use '[defaults]' as a placeholder for all the collectors enabled by default.").
			Default(filterAvailableCollectors(defaultCollectors)).String()
		printCollectors = kingpin.Flag(
			"collectors.print",
			"If true, print available collectors and exit.",
		).Bool()
	)

	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print("wmi_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	if *printCollectors {
		collectorNames := make(sort.StringSlice, 0, len(collector.Factories))
		for n := range collector.Factories {
			collectorNames = append(collectorNames, n)
		}
		collectorNames.Sort()
		fmt.Printf("Available collectors:\n")
		for _, n := range collectorNames {
			fmt.Printf(" - %s\n", n)
		}
		return
	}

	initWbem()

	isInteractive, err := svc.IsAnInteractiveSession()
	if err != nil {
		log.Fatal(err)
	}

	stopCh := make(chan bool)
	if !isInteractive {
		go func() {
			err = svc.Run(serviceName, &wmiExporterService{stopCh: stopCh})
			if err != nil {
				log.Errorf("Failed to start service: %v", err)
			}
		}()
	}

	collectors, err := loadCollectors(*enabledCollectors)
	if err != nil {
		log.Fatalf("Couldn't load collectors: %s", err)
	}

	log.Infof("Enabled collectors: %v", strings.Join(keys(collectors), ", "))

	nodeCollector := WmiCollector{collectors: collectors}
	prometheus.MustRegister(nodeCollector)

	log.Infoln("Starting WMI exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	log.Infoln("Collecting metrics...")
	if err := writeToTextfile(*metricsOutput, prometheus.DefaultGatherer); err != nil {
		log.Fatalf("collection failed: %s", err)
	}
	log.Infoln("Done")

	if !isInteractive {
		for {
			if <-stopCh {
				log.Info("Shutting down WMI exporter")
				break
			}
		}
	}
}

func keys(m map[string]collector.Collector) []string {
	ret := make([]string, 0, len(m))
	for key := range m {
		ret = append(ret, key)
	}
	return ret
}

type wmiExporterService struct {
	stopCh chan<- bool
}

func (s *wmiExporterService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}
loop:
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s.stopCh <- true
				break loop
			default:
				log.Error(fmt.Sprintf("unexpected control request #%d", c))
			}
		}
	}
	changes <- svc.Status{State: svc.StopPending}
	return
}

func writeToTextfile(filename string, g prometheus.Gatherer) error {
	tmp, err := ioutil.TempFile(filepath.Dir(filename), filepath.Base(filename))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	mfs, err := g.Gather()
	if err != nil {
		return err
	}
	for _, mf := range mfs {
		if _, err := expfmt.MetricFamilyToText(tmp, mf); err != nil {
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Chmod(tmp.Name(), 0644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filename)
}
