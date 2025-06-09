package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)

type CepRequest struct {
	Cep string `json:"cep"`
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

	tracer = otel.Tracer("servico-a")

	mux := http.NewServeMux()
	mux.Handle("/cep", otelhttp.NewHandler(http.HandlerFunc(CepHandler), "CepHandler"))

	fmt.Println("Serviço A escutando na porta 8081...")
	log.Fatal(http.ListenAndServe(":8081", mux))
}

func CepHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "Processar CEP")
	defer span.End()

	if r.Method != http.MethodPost {
		http.Error(w, "Método não permitido", http.StatusMethodNotAllowed)
		return
	}

	var req CepRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if !validateCEP(req.Cep) {
		http.Error(w, "invalid zipcode", http.StatusUnprocessableEntity)
		return
	}

	serviceBURL := fmt.Sprintf("http://servico-b:8080/tempo?cep=%s", req.Cep)

	// Instrumenta a chamada HTTP externa
	client := http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	reqB, _ := http.NewRequestWithContext(ctx, "GET", serviceBURL, nil)

	resp, err := client.Do(reqB)
	if err != nil {
		http.Error(w, "erro ao conectar com serviço B", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func validateCEP(cep string) bool {
	re := regexp.MustCompile(`^\d{8}$`)
	return re.MatchString(cep)
}

func initTracer(ctx context.Context) *sdktrace.TracerProvider {
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint("otel-collector:4318"),
		otlptracehttp.WithURLPath("/v1/traces"),

		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		log.Fatalf("Erro ao criar exporter OTLP: %v", err)
	}

	resource := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("servico-a"),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource),
	)

	otel.SetTracerProvider(tp)
	return tp
}
