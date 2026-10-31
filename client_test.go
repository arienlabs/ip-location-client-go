package iploc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(server.URL+"/prefix", WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestLocationContract(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/8.8.8.8" {
			t.Error(r.URL.Path)
		}
		fmt.Fprint(w, `{"country":"PL","stateprov":"Mazowieckie","stateprovCode":"MZ","city":"Warsaw","latitude":"0.0","longitude":"-12.5","continent":"EU","timezone":"Europe/Warsaw","usMetroCode":123,"accuracyRadius":10,"asn":15169,"asnOrganization":"Test","asnNetwork":"8.8.8.0/24"}`)
	})
	location, err := client.Lookup(context.Background(), "8.8.8.8")
	if err != nil {
		t.Fatal(err)
	}
	if location.Country != "PL" || location.Stateprov != "Mazowieckie" || location.StateprovCode != "MZ" || location.City != "Warsaw" || location.Continent != "EU" || location.Timezone != "Europe/Warsaw" || *location.Latitude != 0 || *location.Longitude != -12.5 || *location.ASN != 15169 || *location.AccuracyRadius != 10 || *location.USMetroCode != 123 || location.ASNOrganization != "Test" || location.ASNNetwork != "8.8.8.0/24" {
		t.Fatalf("Niepoprawny wynik: %+v", location)
	}
}

func TestLiteralAddresses(t *testing.T) {
	for _, tc := range []struct{ input, path string }{
		{"8.8.8.8", "8.8.8.8"},
		{"2001:4860:4860::8888", "2001:4860:4860::8888"},
		{"::ffff:8.8.8.8", "8.8.8.8"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/prefix/"+tc.path {
					t.Error(r.URL.Path)
				}
				fmt.Fprint(w, `{"latitude":1.5}`)
			})
			location, err := client.Lookup(context.Background(), tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if *location.Latitude != 1.5 || location.Longitude != nil {
				t.Fatal(location)
			}
		})
	}
}

func TestNotFoundHasNoRetryOrFakeCountry(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(404) })
	location, err := client.Lookup(context.Background(), "10.0.0.1")
	if err != nil || location != nil || calls.Load() != 1 {
		t.Fatalf("%+v %v calls=%d", location, err, calls.Load())
	}
}

func TestServiceErrors(t *testing.T) {
	for _, status := range []int{400, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":"failure"}`)
			})
			_, err := client.Lookup(context.Background(), "8.8.8.8")
			var api *APIError
			if !errors.As(err, &api) || api.StatusCode != status || !strings.Contains(api.ResponseBody, "failure") || calls.Load() != 1 {
				t.Fatal(err)
			}
		})
	}
}

func TestInvalidInputIsLocal(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{}`) })
	for _, ip := range []string{"localhost", "1", "127.1", "127.000.0.1", "0x7f000001", " 8.8.8.8", "fe80::1%eth0", "[::1]", "8.8.8.8/../health", "::ffff:192.168.001.1"} {
		if _, err := client.Lookup(context.Background(), ip); err == nil {
			t.Error("Zaakceptowano", ip)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("Wykonano HTTP dla niepoprawnego IP")
	}
}

func TestBatch(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/prefix/" || r.Header.Get("Content-Type") != "application/json" {
			t.Error(r.Method, r.URL.Path)
		}
		var values []string
		if err := json.NewDecoder(r.Body).Decode(&values); err != nil {
			t.Error(err)
		}
		if len(values) != 2 {
			t.Error(values)
		}
		fmt.Fprint(w, `{"2001:db8:0:0:0:0:0:1":{"country":"PL"},"8.8.8.8":{"asn":15169}}`)
	})
	values, err := client.LookupBatch(context.Background(), []string{"2001:DB8::1", "2001:db8::1", "::ffff:8.8.8.8"})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values["2001:db8::1"].Country != "PL" || *values["8.8.8.8"].ASN != 15169 {
		t.Fatal(values)
	}
}

func TestBatchValidation(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	values, err := client.LookupBatch(context.Background(), nil)
	if err != nil || values == nil || len(values) != 0 {
		t.Fatal(values, err)
	}
	if _, err := client.LookupBatch(context.Background(), make([]string, 101)); err == nil {
		t.Fatal("Zaakceptowano101adresow")
	}
	if _, err := client.LookupBatch(context.Background(), []string{"localhost"}); err == nil {
		t.Fatal("Zaakceptowano nazweDNS")
	}
	if calls.Load() != 0 {
		t.Fatal("NiepotrzebneHTTP")
	}
}

func TestInvalidResponses(t *testing.T) {
	for _, body := range []string{`null`, `{}{}{}`, `{"latitude":"bad"}`, `{"latitude":"NaN"}`, `{"longitude":"+Inf"}`} {
		t.Run(body, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			if _, err := client.Lookup(context.Background(), "8.8.8.8"); err == nil {
				t.Fatal("Zaakceptowano niepoprawny wynik")
			}
		})
	}
	for _, body := range []string{`null`, `{"bad-key":{}}`, `{"8.8.8.8":null}`} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		if _, err := client.LookupBatch(context.Background(), []string{"8.8.8.8"}); err == nil {
			t.Fatal("Zaakceptowano niepoprawny batch")
		}
	}
}

func TestResponseLimit(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat(" ", maxResponseBytes+1)) })
	if _, err := client.Lookup(context.Background(), "8.8.8.8"); err == nil {
		t.Fatal("Brak limitu odpowiedzi")
	}
}

func TestUnavailableHealth(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/health" {
			t.Error(r.URL.Path)
		}
		w.WriteHeader(503)
		fmt.Fprint(w, `{"ready":false,"updateEnabled":true,"cityBuild":"2026-10-27T17:52:02Z"}`)
	})
	value, err := client.Health(context.Background())
	if err != nil || value.Ready || !value.UpdateEnabled || value.CityBuild.Year() != 2026 {
		t.Fatal(value, err)
	}
	for _, body := range []string{`null`, `{}`} {
		malformed := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		if _, err := malformed.Health(context.Background()); err == nil {
			t.Fatal("Zaakceptowano niepoprawny health")
		}
	}
}

func TestCancellationAndConcurrentUse(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"country":"US"}`) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Lookup(ctx, "8.8.8.8"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := client.LookupBatch(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := client.Lookup(context.Background(), "8.8.8.8")
			if err != nil || value.Country != "US" {
				t.Error(value, err)
			}
		}()
	}
	wg.Wait()
}

func TestDeadline(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := client.Lookup(ctx, "8.8.8.8"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestConfiguration(t *testing.T) {
	for _, url := range []string{"", "ftp://example.com", "http://user:password@example.com", "http://example.com/?q=1", "http://example.com/#fragment"} {
		if _, err := New(url); err == nil {
			t.Error("Zaakceptowano", url)
		}
	}
	if _, err := New("http://example.com", WithHTTPClient(nil)); err == nil {
		t.Fatal("Zaakceptowano nil httpclient")
	}
	if _, err := New("http://example.com", nil); err == nil {
		t.Fatal("Zaakceptowano nil opcje")
	}
	client, err := New("http://example.com")
	if err != nil || client.http.Timeout != 5*time.Second {
		t.Fatal(client, err)
	}
}
