FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY templates ./templates
RUN go build -o /out/license-backend .

FROM alpine:3.20
WORKDIR /app
COPY --from=builder /out/license-backend /app/license-backend
COPY templates /app/templates
RUN mkdir -p /app/data
EXPOSE 8085
ENV ADDR=:8085
ENV DATA_FILE=/app/data/license-db.json
CMD ["/app/license-backend"]
