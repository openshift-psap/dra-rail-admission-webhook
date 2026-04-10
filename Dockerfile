FROM golang:1.25 AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o webhook ./cmd/webhook/
RUN CGO_ENABLED=0 GOOS=linux go build -o reconciler ./cmd/reconciler/

# --- Webhook image ---
FROM gcr.io/distroless/static:nonroot AS webhook
WORKDIR /
COPY --from=builder /workspace/webhook .
USER 65532:65532
ENTRYPOINT ["/webhook"]

# --- Reconciler image ---
FROM gcr.io/distroless/static:nonroot AS reconciler
WORKDIR /
COPY --from=builder /workspace/reconciler .
USER 65532:65532
ENTRYPOINT ["/reconciler"]
