package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)

type ViaCEP struct {
	Localidade string `json:"localidade"`
	Uf         string `json:"uf"`
}

type OpenMeteoResponse struct {
	CurrentWeather struct {
		Temperature float64 `json:"temperature"`
	} `json:"current_weather"`
}

type TempResp struct {
	City  string  `json:"city"`
	TempC float64 `json:"temp_C"`
	TempF float64 `json:"temp_F"`
	TempK float64 `json:"temp_K"`
}

var tracer trace.Tracer

func main() {
	ctx := context.Background()
	tp := initTracer(ctx)
	defer func() {
		if err := tp.Shutdown(ctx); err != nil {
			log.Fatalf("Erro ao encerrar tracer: %v", err)
		}
	}()

	tracer = otel.Tracer("servico-b")

	mux := http.NewServeMux()
	mux.Handle("/tempo", otelhttp.NewHandler(http.HandlerFunc(WeatherHandler), "WeatherHandler"))

	fmt.Println("Serviço B escutando na porta 8080...")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func WeatherHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "Processar tempo por CEP")
	defer span.End()

	// Log para debug
	spanCtx := trace.SpanContextFromContext(ctx)
	log.Printf("TraceID: %s, SpanID: %s", spanCtx.TraceID().String(), spanCtx.SpanID().String())

	cepParam := r.URL.Query().Get("cep")
	if !validateCEP(cepParam) {
		http.Error(w, "invalid zipcode", http.StatusUnprocessableEntity)
		return
	}

	cepData, err := BuscaCEP(ctx, cepParam)
	if err != nil || cepData.Localidade == "" {
		log.Printf("CEP não encontrado: %v", err)
		http.Error(w, "can not find zipcode", http.StatusNotFound)
		return
	}

	tempC, err := fetchWeather(ctx, cepData.Localidade)
	if err != nil {
		log.Printf("Erro ao buscar temperatura: %v", err)
		http.Error(w, "erro ao buscar temperatura", http.StatusInternalServerError)
		return
	}

	temps := convertTemps(tempC, cepData.Localidade)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(temps)
}

func validateCEP(cep string) bool {
	re := regexp.MustCompile(`^\d{8}$`)
	return re.MatchString(cep)
}

func BuscaCEP(ctx context.Context, cep string) (*ViaCEP, error) {
	ctx, span := tracer.Start(ctx, "ViaCEP API")
	defer span.End()

	client := http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://viacep.com.br/ws/"+cep+"/json/", nil)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var c ViaCEP
	err = json.Unmarshal(body, &c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func fetchWeather(ctx context.Context, cidade string) (float64, error) {
	ctx, span := tracer.Start(ctx, "Buscar clima Open-Meteo")
	defer span.End()

	client := http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}

	geoAPIURL := fmt.Sprintf("https://geocoding-api.open-meteo.com/v1/search?name=%s&count=1&language=pt&format=json", url.QueryEscape(cidade))
	reqGeo, _ := http.NewRequestWithContext(ctx, "GET", geoAPIURL, nil)
	respGeo, err := client.Do(reqGeo)
	if err != nil {
		return 0, err
	}
	defer respGeo.Body.Close()

	var geoResult struct {
		Results []struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"results"`
	}

	bodyGeo, err := io.ReadAll(respGeo.Body)
	if err != nil {
		return 0, err
	}

	if err := json.Unmarshal(bodyGeo, &geoResult); err != nil {
		return 0, err
	}

	if len(geoResult.Results) == 0 {
		return 0, fmt.Errorf("não foi possível encontrar coordenadas para a cidade: %s", cidade)
	}

	latitude := geoResult.Results[0].Latitude
	longitude := geoResult.Results[0].Longitude

	weatherURL := fmt.Sprintf("https://api.open-meteo.com/v1/forecast?latitude=%.6f&longitude=%.6f&current_weather=true", latitude, longitude)
	reqWeather, _ := http.NewRequestWithContext(ctx, "GET", weatherURL, nil)
	respWeather, err := client.Do(reqWeather)
	if err != nil {
		return 0, err
	}
	defer respWeather.Body.Close()

	var weatherData OpenMeteoResponse
	body, err := io.ReadAll(respWeather.Body)
	if err != nil {
		return 0, err
	}
	err = json.Unmarshal(body, &weatherData)
	if err != nil {
		return 0, err
	}

	return weatherData.CurrentWeather.Temperature, nil
}

func convertTemps(celsius float64, cidade string) TempResp {
	return TempResp{
		City:  cidade,
		TempC: celsius,
		TempF: celsius*1.8 + 32,
		TempK: celsius + 273,
	}
}

func initTracer(ctx context.Context) *sdktrace.TracerProvider {
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint("http://otel-collector:4318"),
		otlptracehttp.WithURLPath("/v1/traces"),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		log.Fatalf("Erro ao criar exporter OTLP: %v", err)
	}

	resource := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("servico-b"),
		semconv.ServiceVersion("1.0.0"),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource),
	)

	otel.SetTracerProvider(tp)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp
}
