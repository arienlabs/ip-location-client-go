// Package iploc udostępnia klienta HTTP usługi Arienlabs IP Location API.
package iploc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 1 << 20

// Location zawiera dostępne dane lokalizacji i ASN. Brak współrzędnej to nil, a nie zero.
type Location struct {
	Country         string   `json:"country,omitempty"`
	Stateprov       string   `json:"stateprov,omitempty"`
	StateprovCode   string   `json:"stateprovCode,omitempty"`
	City            string   `json:"city,omitempty"`
	Latitude        *float64 `json:"latitude,omitempty"`
	Longitude       *float64 `json:"longitude,omitempty"`
	Continent       string   `json:"continent,omitempty"`
	Timezone        string   `json:"timezone,omitempty"`
	USMetroCode     *uint32  `json:"usMetroCode,omitempty"`
	AccuracyRadius  *uint32  `json:"accuracyRadius,omitempty"`
	ASN             *uint32  `json:"asn,omitempty"`
	ASNOrganization string   `json:"asnOrganization,omitempty"`
	ASNNetwork      string   `json:"asnNetwork,omitempty"`
}

// UnmarshalJSON obsługuje współrzędne zapisane jako liczby lub ciągi znaków.
func (l *Location) UnmarshalJSON(data []byte) error {
	type plain Location
	var value Location
	wire := struct {
		*plain
		Latitude  json.RawMessage `json:"latitude"`
		Longitude json.RawMessage `json:"longitude"`
	}{plain: (*plain)(&value)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	var err error
	if value.Latitude, err = coordinate(wire.Latitude); err != nil {
		return fmt.Errorf("latitude: %w", err)
	}
	if value.Longitude, err = coordinate(wire.Longitude); err != nil {
		return fmt.Errorf("longitude: %w", err)
	}
	*l = value
	return nil
}

func coordinate(raw json.RawMessage) (*float64, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	text := string(raw)
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, err
		}
	}
	number, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil, errors.New("nieprawidlowa wspolrzedna")
	}
	return &number, nil
}

// Health opisuje gotowość API i aktualizacje baz.
type Health struct {
	Ready               bool       `json:"ready"`
	UpdateEnabled       bool       `json:"updateEnabled"`
	LastError           string     `json:"lastError"`
	Generation          string     `json:"generation"`
	CityBuild           *time.Time `json:"cityBuild,omitempty"`
	ASNBuild            *time.Time `json:"asnBuild,omitempty"`
	LastAttempt         *time.Time `json:"lastAttempt,omitempty"`
	LastSuccessfulCheck *time.Time `json:"lastSuccessfulCheck,omitempty"`
}

func (h *Health) UnmarshalJSON(data []byte) error {
	type plain Health
	var value Health
	wire := struct {
		*plain
		Ready *bool `json:"ready"`
	}{plain: (*plain)(&value)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Ready == nil {
		return errors.New("odpowiedz health nie zawiera pola ready")
	}
	value.Ready = *wire.Ready
	*h = value
	return nil
}

// APIError zachowuje kod HTTP i ograniczoną rozmiarem treść odpowiedzi błędu.
type APIError struct {
	StatusCode   int
	ResponseBody string
}

func (e *APIError) Error() string { return fmt.Sprintf("IP Location API: HTTP %d", e.StatusCode) }

// Client można współdzielić między goroutines. Nie zamyka klienta HTTP przekazanego przez użytkownika.
type Client struct {
	base *url.URL
	http *http.Client
}

type Option func(*Client) error

// WithHTTPClient zastępuje domyślnego klienta; timeout i transport należą wtedy do wywołującego.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) error {
		if client == nil {
			return errors.New("http.Client nie moze byc nil")
		}
		c.http = client
		return nil
	}
}

// New tworzy klienta z domyślnym limitem czasu5 sekund i bez automatycznych ponowień.
func New(baseURL string, options ...Option) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil || !base.IsAbs() || base.Hostname() == "" || (base.Scheme != "http" && base.Scheme != "https") || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.User != nil {
		return nil, errors.New("wymagany URL HTTP/HTTPS bez query, fragmentu i danych logowania")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/"
	if base.RawPath != "" {
		base.RawPath = strings.TrimRight(base.RawPath, "/") + "/"
	}
	client := &Client{base: base, http: &http.Client{Timeout: 5 * time.Second}}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("opcja nie moze byc nil")
		}
		if err := option(client); err != nil {
			return nil, err
		}
	}
	return client, nil
}

func normalize(value string) (string, error) {
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" {
		return "", fmt.Errorf("nieprawidlowy adres IP: %q", value)
	}
	return ip.Unmap().String(), nil
}

func (c *Client) send(ctx context.Context, method, path string, input any) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	uri := c.base.ResolveReference(&url.URL{Path: path})
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, uri.String(), body)
	if err != nil {
		return nil, err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	return c.http.Do(request)
}

func readBody(response *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("odpowiedz API przekracza 1MiB")
	}
	return body, nil
}

func decode(response *http.Response, destination any, allowUnavailable bool) error {
	data, err := readBody(response)
	if err != nil {
		return err
	}
	if (response.StatusCode < 200 || response.StatusCode >= 300) && !(allowUnavailable && response.StatusCode == http.StatusServiceUnavailable) {
		return &APIError{StatusCode: response.StatusCode, ResponseBody: string(data)}
	}
	return json.Unmarshal(data, destination)
}

// Lookup zwraca nil,nil wyłącznie dla odpowiedzi HTTP404. Prywatne adresy nie dostają fikcyjnej lokalizacji.
func (c *Client) Lookup(ctx context.Context, ip string) (*Location, error) {
	value, err := normalize(ip)
	if err != nil {
		return nil, err
	}
	response, err := c.send(ctx, http.MethodGet, value, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	var location *Location
	if err := decode(response, &location, false); err != nil {
		return nil, err
	}
	if location == nil {
		return nil, errors.New("API zwrocilo null zamiast lokalizacji")
	}
	return location, nil
}

// LookupBatch obsługuje do100 wejść; klucze wyniku są kanoniczne i bez duplikatów.
func (c *Client) LookupBatch(ctx context.Context, ips []string) (map[string]Location, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ips) > 100 {
		return nil, errors.New("API obsluguje maksymalnie 100 adresow")
	}
	values := make([]string, 0, len(ips))
	seen := make(map[string]bool, len(ips))
	for _, ip := range ips {
		value, err := normalize(ip)
		if err != nil {
			return nil, err
		}
		if !seen[value] {
			seen[value] = true
			values = append(values, value)
		}
	}
	result := make(map[string]Location)
	if len(values) == 0 {
		return result, nil
	}
	response, err := c.send(ctx, http.MethodPost, "", values)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var wire map[string]*Location
	if err := decode(response, &wire, false); err != nil {
		return nil, err
	}
	if wire == nil {
		return nil, errors.New("API zwrocilo null zamiast mapy")
	}
	for key, value := range wire {
		normalized, err := normalize(key)
		if err != nil || value == nil {
			return nil, errors.New("nieprawidlowy element batch w odpowiedzi API")
		}
		result[normalized] = *value
	}
	return result, nil
}

// Health zwraca również prawidłową odpowiedź ready=false zHTTP503.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	response, err := c.send(ctx, http.MethodGet, "health", nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var value *Health
	if err := decode(response, &value, true); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("API zwrocilo null zamiast stanu")
	}
	return value, nil
}
