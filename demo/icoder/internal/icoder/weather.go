package icoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type WeatherProvider interface {
	Current(context.Context, string, string) (Weather, error)
}

type Weather struct {
	Location        string  `json:"location"`
	Country         string  `json:"country,omitempty"`
	Latitude        float64 `json:"latitude"`
	Longitude       float64 `json:"longitude"`
	Temperature     float64 `json:"temperature"`
	Apparent        float64 `json:"apparent_temperature"`
	Humidity        float64 `json:"relative_humidity_percent"`
	Precipitation   float64 `json:"precipitation"`
	WindSpeed       float64 `json:"wind_speed"`
	WeatherCode     int     `json:"weather_code"`
	Condition       string  `json:"condition"`
	TemperatureUnit string  `json:"temperature_unit"`
	WindSpeedUnit   string  `json:"wind_speed_unit"`
	ObservedAt      string  `json:"observed_at"`
}

type OpenMeteoWeatherProvider struct {
	client       *http.Client
	geocodingURL string
	forecastURL  string
}

func NewOpenMeteoWeatherProvider(client *http.Client) *OpenMeteoWeatherProvider {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &OpenMeteoWeatherProvider{
		client:       client,
		geocodingURL: "https://geocoding-api.open-meteo.com/v1/search",
		forecastURL:  "https://api.open-meteo.com/v1/forecast",
	}
}

func (p *OpenMeteoWeatherProvider) Current(ctx context.Context, location, units string) (Weather, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return Weather{}, fmt.Errorf("location is required")
	}
	if units != "celsius" && units != "fahrenheit" {
		return Weather{}, fmt.Errorf("units must be celsius or fahrenheit")
	}
	geocoding, err := p.geocode(ctx, location)
	if err != nil {
		return Weather{}, err
	}
	return p.forecast(ctx, geocoding, units)
}

type geocodingResult struct {
	Name      string  `json:"name"`
	Country   string  `json:"country"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

func (p *OpenMeteoWeatherProvider) geocode(ctx context.Context, location string) (geocodingResult, error) {
	query := url.Values{"name": {location}, "count": {"1"}, "language": {"en"}, "format": {"json"}}
	var response struct {
		Results []geocodingResult `json:"results"`
	}
	if err := p.getJSON(ctx, p.geocodingURL+"?"+query.Encode(), &response); err != nil {
		return geocodingResult{}, fmt.Errorf("geocode location: %w", err)
	}
	if len(response.Results) == 0 {
		return geocodingResult{}, fmt.Errorf("location %q was not found", location)
	}
	return response.Results[0], nil
}

func (p *OpenMeteoWeatherProvider) forecast(ctx context.Context, location geocodingResult, units string) (Weather, error) {
	query := url.Values{
		"latitude":  {fmt.Sprintf("%g", location.Latitude)},
		"longitude": {fmt.Sprintf("%g", location.Longitude)},
		"current":   {"temperature_2m,apparent_temperature,relative_humidity_2m,precipitation,weather_code,wind_speed_10m"},
		"timezone":  {"auto"},
	}
	if units == "fahrenheit" {
		query.Set("temperature_unit", "fahrenheit")
		query.Set("wind_speed_unit", "mph")
		query.Set("precipitation_unit", "inch")
	}
	var response struct {
		Current struct {
			Time                string  `json:"time"`
			Temperature         float64 `json:"temperature_2m"`
			ApparentTemperature float64 `json:"apparent_temperature"`
			Humidity            float64 `json:"relative_humidity_2m"`
			Precipitation       float64 `json:"precipitation"`
			WeatherCode         int     `json:"weather_code"`
			WindSpeed           float64 `json:"wind_speed_10m"`
		} `json:"current"`
		Units struct {
			Temperature string `json:"temperature_2m"`
			WindSpeed   string `json:"wind_speed_10m"`
		} `json:"current_units"`
	}
	if err := p.getJSON(ctx, p.forecastURL+"?"+query.Encode(), &response); err != nil {
		return Weather{}, fmt.Errorf("get forecast: %w", err)
	}
	return Weather{
		Location: location.Name, Country: location.Country, Latitude: location.Latitude, Longitude: location.Longitude,
		Temperature: response.Current.Temperature, Apparent: response.Current.ApparentTemperature,
		Humidity: response.Current.Humidity, Precipitation: response.Current.Precipitation,
		WindSpeed: response.Current.WindSpeed, WeatherCode: response.Current.WeatherCode,
		Condition: weatherCondition(response.Current.WeatherCode), TemperatureUnit: response.Units.Temperature,
		WindSpeedUnit: response.Units.WindSpeed, ObservedAt: response.Current.Time,
	}, nil
}

func (p *OpenMeteoWeatherProvider) getJSON(ctx context.Context, endpoint string, output any) (resultErr error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, response.Body.Close())
	}()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("weather service returned HTTP %d", response.StatusCode)
	}
	if err := json.Unmarshal(body, output); err != nil {
		return fmt.Errorf("decode weather response: %w", err)
	}
	return nil
}

type weatherTool struct{ provider WeatherProvider }

func (weatherTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "get_weather", Description: "Get current weather for a city or place name using Open-Meteo.", Strict: true,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"location": map[string]any{"type": "string", "description": "City or place name, for example Beijing or Paris"},
				"units":    map[string]any{"type": "string", "enum": []any{"celsius", "fahrenheit"}},
			},
			"required": []any{"location", "units"},
		},
	}
}
func (weatherTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t weatherTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Location string `json:"location"`
		Units    string `json:"units"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	weather, err := t.provider.Current(ctx, input.Location, input.Units)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	content, err := marshalString(weather)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: content}, nil
}

func weatherCondition(code int) string {
	switch {
	case code == 0:
		return "clear sky"
	case code <= 3:
		return "partly cloudy"
	case code == 45 || code == 48:
		return "fog"
	case code >= 51 && code <= 57:
		return "drizzle"
	case code >= 61 && code <= 67:
		return "rain"
	case code >= 71 && code <= 77:
		return "snow"
	case code >= 80 && code <= 82:
		return "rain showers"
	case code >= 85 && code <= 86:
		return "snow showers"
	case code >= 95:
		return "thunderstorm"
	default:
		return "unknown"
	}
}
