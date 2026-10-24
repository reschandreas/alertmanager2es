package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elastic/go-elasticsearch/v9"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

type esRequest struct {
	method string
	path   string
	body   []byte
}

type fakeElasticsearch struct {
	mu       sync.Mutex
	requests []esRequest
	status   int
}

func (f *fakeElasticsearch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	f.mu.Lock()
	f.requests = append(f.requests, esRequest{method: r.Method, path: r.URL.Path, body: body})
	status := f.status
	f.mu.Unlock()

	if status == 0 {
		status = http.StatusCreated
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Elastic-Product", "Elasticsearch")

	if r.Method == http.MethodGet && r.URL.Path == "/" {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"name":"test","version":{"number":"9.5.5"},"tagline":"You Know, for Search"}`)
		return
	}

	w.WriteHeader(status)
	if status == http.StatusCreated {
		_, _ = io.WriteString(w, `{"result":"created"}`)
	}
}

func (f *fakeElasticsearch) recorded() []esRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	requests := make([]esRequest, len(f.requests))
	copy(requests, f.requests)
	return requests
}

func newTestExporter(t *testing.T, handler http.Handler) *AlertmanagerElasticsearchExporter {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := elasticsearch.New(elasticsearch.WithAddresses(server.URL))
	if err != nil {
		t.Fatalf("create elasticsearch client: %v", err)
	}

	exporter := &AlertmanagerElasticsearchExporter{
		elasticSearchClient:    client,
		elasticsearchIndexName: "alertmanager-%y.%m.%d",
	}
	// Init() registers metrics globally; tests only need the counters to exist.
	exporter.prometheus.alertsReceived = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_alerts_received"}, nil)
	exporter.prometheus.alertsInvalid = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_alerts_invalid"}, nil)
	exporter.prometheus.alertsSuccessful = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_alerts_successful"}, nil)

	return exporter
}

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()

	var metric dto.Metric
	if err := counter.Write(&metric); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return metric.GetCounter().GetValue()
}

func TestBuildIndexName(t *testing.T) {
	exporter := &AlertmanagerElasticsearchExporter{elasticsearchIndexName: "alertmanager-%y.%m.%d"}

	got := exporter.buildIndexName(time.Date(2026, time.October, 24, 15, 4, 5, 0, time.UTC))
	if want := "alertmanager-2026.10.24"; got != want {
		t.Fatalf("buildIndexName() = %q, want %q", got, want)
	}
}

func TestHttpHandlerRejectsInvalidRequests(t *testing.T) {
	exporter := newTestExporter(t, http.NotFoundHandler())

	tests := []struct {
		name       string
		method     string
		body       string
		removeBody bool
		wantStatus int
	}{
		{
			name:       "method not allowed",
			method:     http.MethodGet,
			body:       `{}`,
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "empty body",
			method:     http.MethodPost,
			removeBody: true,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid json",
			method:     http.MethodPost,
			body:       `{`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unsupported webhook version",
			method:     http.MethodPost,
			body:       `{"version":"3"}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/webhook", strings.NewReader(test.body))
			if test.removeBody {
				request.Body = nil
			}

			response := httptest.NewRecorder()
			exporter.HttpHandler(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("HttpHandler() status = %d, want %d (body: %s)", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func TestHttpHandlerIndexesEveryAlert(t *testing.T) {
	es := &fakeElasticsearch{}
	exporter := newTestExporter(t, es)

	payload, err := os.ReadFile("example.json")
	if err != nil {
		t.Fatalf("read example.json: %v", err)
	}

	var notification AlertmanagerEntry
	if err := json.Unmarshal(payload, &notification); err != nil {
		t.Fatalf("decode example.json: %v", err)
	}
	if len(notification.Alerts) == 0 {
		t.Fatal("example.json contains no alerts")
	}

	request := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload))
	response := httptest.NewRecorder()
	exporter.HttpHandler(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("HttpHandler() status = %d, want %d (body: %s)", response.Code, http.StatusOK, response.Body.String())
	}

	// The Elasticsearch client performs a one-off product check against GET /.
	var requests []esRequest
	for _, request := range es.recorded() {
		if strings.HasSuffix(request.path, "/_doc") {
			requests = append(requests, request)
		}
	}
	if len(requests) != len(notification.Alerts) {
		for i, request := range requests {
			t.Logf("request %d: %s %s", i, request.method, request.path)
		}
		t.Fatalf("indexed %d documents, want %d", len(requests), len(notification.Alerts))
	}

	indexPath := regexp.MustCompile(`^/alertmanager-\d{4}\.\d{2}\.\d{2}/_doc$`)
	for i, indexed := range requests {
		if indexed.method != http.MethodPost {
			t.Errorf("request %d used method %q, want %q", i, indexed.method, http.MethodPost)
		}
		if !indexPath.MatchString(indexed.path) {
			t.Errorf("request %d used path %q, want an index document path", i, indexed.path)
		}

		var document map[string]any
		if err := json.Unmarshal(indexed.body, &document); err != nil {
			t.Fatalf("decode indexed document %d: %v", i, err)
		}
		if document["version"] != supportedWebhookVersion {
			t.Errorf("document %d version = %v, want %q", i, document["version"], supportedWebhookVersion)
		}
		if document["groupKey"] != notification.GroupKey {
			t.Errorf("document %d groupKey = %v, want %q", i, document["groupKey"], notification.GroupKey)
		}
		if _, ok := document["@timestamp"]; !ok {
			t.Errorf("document %d is missing @timestamp", i)
		}

		alert, ok := document["Alert"].(map[string]any)
		if !ok {
			t.Fatalf("document %d has no Alert object: %v", i, document)
		}
		if alert["status"] != "firing" {
			t.Errorf("document %d alert status = %v, want %q", i, alert["status"], "firing")
		}
	}

	if got := counterValue(t, exporter.prometheus.alertsSuccessful.WithLabelValues()); got != float64(len(requests)) {
		t.Errorf("alertsSuccessful counter = %v, want %d", got, len(requests))
	}
}
