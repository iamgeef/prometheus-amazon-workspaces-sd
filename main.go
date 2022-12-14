// Copyright 2018 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/workspaces"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/version"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	a           = kingpin.New("prometheus-amazon-workspaces-sd", "Tool to generate file_sd target files for Amazon Workspaces.")
	outputFile  = a.Flag("output.file", "Output file for file_sd compatible file.").Default("workspaces_sd.json").String()
	refresh     = a.Flag("target.refresh", "The refresh interval (in seconds).").Default("86400").Int()
	profile     = a.Flag("profile", "AWS Profile").Default("").String()
	listen      = a.Flag("web.listen-address", "The listen address.").Default(":9888").String()
	metricsPath = a.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
	pages = a.Flag("pages","Number of results pages to iterate. 1 page = 25 Workspaces.").Default("1").Int()
	exporterPort = a.Flag("exporterPort", "Port used by the installed exporter").Default("9182").String()

	logger log.Logger
	sess   client.ConfigProvider

	userNameLabel = model.MetaLabelPrefix + "workspaces_username"
	subnetIdLabel      = model.MetaLabelPrefix + "workspaces_subnet_id"
	stateLabel         = model.MetaLabelPrefix + "workspaces_state"
	directoryIdLabel       = model.MetaLabelPrefix + "workspaces_directory_id"
	bundleIdLabel             = model.MetaLabelPrefix + "workspaces_bundle_id"
	computeTypeLabel        = model.MetaLabelPrefix + "workspaces_compute_type"
	runningModeLabel         = model.MetaLabelPrefix + "workspaces_running_mode"
	rootVolumeSizeLabel            = model.MetaLabelPrefix + "workspaces_root_volume_size"
	userVolumeSizeLabel            = model.MetaLabelPrefix + "workspaces_user_volume_size"
	AddressLabel      = model.MetaLabelPrefix + "workspaces_ip_address"
	tagLabel              = model.MetaLabelPrefix + "workspaces_tag_"
)

var (
	reg             = prometheus.NewRegistry()
	requestDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "prometheus_workspaces_sd_request_duration_seconds",
			Help:    "Histogram of latencies for requests to the Amazon Workspaces API.",
			Buckets: []float64{0.001, 0.01, 0.1, 0.5, 1.0, 2.0, 5.0, 10.0},
		},
	)
	discoveredTargets = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "prometheus_workspaces_sd_discovered_targets",
			Help: "Number of discovered workspaces targets",
		},
	)
	requestFailures = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "prometheus_workspaces_sd_request_failures_total",
			Help: "Total number of failed requests to the Amazon Workspaces API.",
		},
	)
)

func init() {
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	reg.MustRegister(prometheus.NewGoCollector())
	reg.MustRegister(version.NewCollector("prometheus_workspaces_sd"))
	reg.MustRegister(requestDuration)
	reg.MustRegister(discoveredTargets)
	reg.MustRegister(requestFailures)
}

type workspacesDiscoverer struct {
	client  *workspaces.WorkSpaces
	refresh int
	logger  log.Logger
	lasts   map[string]struct{}
}

func (d *workspacesDiscoverer) createTarget(srv workspaces.Workspace) *targetgroup.Group {
	// level.Debug(d.logger).Log("msg", "creating tg", "tg", srv)
	if srv.IpAddress == nil {
		nullMessage := "null"
		srv.IpAddress = &nullMessage
		srv.SubnetId = &nullMessage
	}
		// create targetgroup
	tg := &targetgroup.Group{
		Source: fmt.Sprintf("workspaces/%s", *srv.WorkspaceId),
		Targets: []model.LabelSet{
			model.LabelSet{
				model.AddressLabel: model.LabelValue(*srv.IpAddress+":"+*exporterPort),
			},
		},
		Labels: model.LabelSet{
			model.AddressLabel:                     model.LabelValue(*srv.IpAddress),
			model.LabelName(userNameLabel): 		model.LabelValue(*srv.UserName),
			model.LabelName(subnetIdLabel):      	model.LabelValue(*srv.SubnetId),
			model.LabelName(stateLabel):        	 model.LabelValue(*srv.State),
			model.LabelName(directoryIdLabel):             model.LabelValue(*srv.DirectoryId),
			model.LabelName(bundleIdLabel):        model.LabelValue(*srv.BundleId),
			model.LabelName(computeTypeLabel):         model.LabelValue(*srv.WorkspaceProperties.ComputeTypeName),
			model.LabelName(runningModeLabel):            model.LabelValue(*srv.WorkspaceProperties.RunningMode),
			model.LabelName(rootVolumeSizeLabel):      model.LabelValue(strconv.FormatInt(int64(*srv.WorkspaceProperties.RootVolumeSizeGib), 10)),
			model.LabelName(userVolumeSizeLabel):      model.LabelValue(strconv.FormatInt(int64(*srv.WorkspaceProperties.UserVolumeSizeGib), 10)),
		},
	}
	return tg
}

func (d *workspacesDiscoverer) getTargets() ([]*targetgroup.Group, error) {
	now := time.Now()

	params := &workspaces.DescribeWorkspacesInput{
	}
	pageNum := 0
	var srvs []workspaces.Workspace
	err := d.client.DescribeWorkspacesPages(params,
		func(page *workspaces.DescribeWorkspacesOutput, lastPage bool) bool {
			pageNum++
				for _,s := range page.Workspaces {
					srvs = append(srvs,*s)
				}
			return pageNum <= *pages - 1
		})
	requestDuration.Observe(time.Since(now).Seconds())

	if err != nil {
		return nil, err
	}

	// create targetgroups from workspaces
	level.Info(d.logger).Log("msg", srvs)
	discoveredTargets.Set(float64(len(srvs)))
	level.Debug(d.logger).Log("msg", "get servers", "count", len(srvs))

	current := make(map[string]struct{})
	tgs := make([]*targetgroup.Group, len(srvs))
	for _, s := range srvs {
		tg := d.createTarget(s)
		level.Debug(d.logger).Log("msg", "server added", "source", tg.Source)
		current[tg.Source] = struct{}{}
		tgs = append(tgs, tg)
	}

	// add empty groups for servers which have been removed since the last refresh
	for k := range d.lasts {
		if _, ok := current[k]; !ok {
			level.Debug(d.logger).Log("msg", "server deleted", "source", k)
			tgs = append(tgs, &targetgroup.Group{Source: k})
		}
	}

	d.lasts = current
	return tgs, nil
}

func (d *workspacesDiscoverer) Run(ctx context.Context, ch chan<- []*targetgroup.Group) {
	for c := time.Tick(time.Duration(d.refresh) * time.Second); ; {
		tgs, err := d.getTargets()

		if err == nil {
			ch <- tgs
		} else {
			// increment failure metric
			requestFailures.Inc()
			level.Error(logger).Log("msg", "error fetching targets", "err", err)
		}

		// wait for ticker or exit when ctx is closed
		select {
		case <-c:
			continue
		case <-ctx.Done():
			return
		}
	}
}

func main() {
	a.HelpFlag.Short('h')
	a.Version(version.Print("prometheus-amazon-workspaces-sd"))

	logger = log.NewSyncLogger(log.NewLogfmtLogger(os.Stdout))
	logger = log.With(logger, "ts", log.DefaultTimestampUTC, "caller", log.DefaultCaller)

	_, err := a.Parse(os.Args[1:])
	if err != nil {
		level.Error(logger).Log("msg", err)
		return
	}

	// use aws named profile if specified, otherwise use NewSession()
	if *profile != "" {
		level.Debug(logger).Log("msg", "loading profile: "+*profile)
		sess, err = session.NewSessionWithOptions(session.Options{
			Config: aws.Config{
				MaxRetries:                    aws.Int(3),
				CredentialsChainVerboseErrors: aws.Bool(true),
				Region:      aws.String("ap-southeast-2"),
				HTTPClient:                    &http.Client{Timeout: 10 * time.Second},
			},
			Profile:           *profile,
			SharedConfigState: session.SharedConfigEnable,
		})
		if err != nil {
			level.Error(logger).Log("msg", "error creating session", "err", err)
			return
		}
	} else {
		level.Debug(logger).Log("msg", "loading shared config")
		sess, err = session.NewSession(&aws.Config{
			MaxRetries:                    aws.Int(3),
			CredentialsChainVerboseErrors: aws.Bool(true),
			Region:      aws.String("ap-southeast-2"),
			HTTPClient:                    &http.Client{Timeout: 10 * time.Second},
		})
		if err != nil {
			level.Error(logger).Log("msg", "error creating session", "err", err)
			return
		}
	}

	workspacesClient := workspaces.New(sess)

	ctx := context.Background()

	disc := &workspacesDiscoverer{
		client:  workspacesClient,
		refresh: *refresh,
		logger:  logger,
		lasts:   make(map[string]struct{}),
	}

	sdAdapter := NewAdapter(ctx, *outputFile, "workspacesSD", disc, logger)
	sdAdapter.Run()

	level.Debug(logger).Log("msg", "listening for connections", "addr", *listen)
	http.Handle(*metricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
		<head><title>prometheus-amazon-workspaces-sd</title></head>
		<body>
		<h1>prometheus-amazon-workspaces-sd</h1>
		<p><a href="` + *metricsPath + `">Metrics</a></p>
		</body>
		</html>`))
	})
	if err := http.ListenAndServe(*listen, nil); err != nil {
		level.Debug(logger).Log("msg", "failed to listen", "addr", *listen, "err", err)
		os.Exit(1)
	}
}
