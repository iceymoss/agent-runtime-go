package icoder

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
)

func TestOpenMeteoWeatherProviderCurrent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		var payload string
		switch request.URL.Path {
		case "/search":
			if request.URL.Query().Get("name") != "Beijing" {
				t.Errorf("geocoding query = %q", request.URL.RawQuery)
			}
			payload = `{"results":[{"name":"Beijing","country":"China","latitude":39.9042,"longitude":116.4074}]}`
		case "/forecast":
			if !strings.Contains(request.URL.Query().Get("current"), "weather_code") {
				t.Errorf("forecast query = %q", request.URL.RawQuery)
			}
			payload = `{"current":{"time":"2026-08-04T10:00","temperature_2m":28.5,"apparent_temperature":30.1,"relative_humidity_2m":61,"precipitation":0,"weather_code":1,"wind_speed_10m":8.2},"current_units":{"temperature_2m":"°C","wind_speed_10m":"km/h"}}`
		default:
			response.WriteHeader(http.StatusNotFound)
			return
		}
		if _, err := response.Write([]byte(payload)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	provider := NewOpenMeteoWeatherProvider(server.Client())
	provider.geocodingURL = server.URL + "/search"
	provider.forecastURL = server.URL + "/forecast"
	weather, err := provider.Current(context.Background(), " Beijing ", "celsius")
	if err != nil {
		t.Fatal(err)
	}
	if weather.Location != "Beijing" || weather.Country != "China" || weather.Condition != "partly cloudy" || weather.Temperature != 28.5 || weather.TemperatureUnit != "°C" {
		t.Fatalf("weather = %#v", weather)
	}
}

func TestWeatherToolReturnsModelVisibleLookupError(t *testing.T) {
	tool := weatherTool{provider: failingWeatherProvider{}}
	result, err := tool.Execute(context.Background(), agent.ToolInvocation{RawInput: `{"location":"missing","units":"celsius"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Content != "lookup failed" {
		t.Fatalf("result = %#v", result)
	}
}

func TestOpenMeteoWeatherProviderRejectsUnits(t *testing.T) {
	provider := NewOpenMeteoWeatherProvider(nil)
	if _, err := provider.Current(context.Background(), "Beijing", "kelvin"); err == nil {
		t.Fatal("Current() accepted unsupported units")
	}
}

type failingWeatherProvider struct{}

func (failingWeatherProvider) Current(context.Context, string, string) (Weather, error) {
	return Weather{}, errWeatherFixture
}

var errWeatherFixture = errors.New("lookup failed")
