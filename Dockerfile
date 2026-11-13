FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /agent-mail ./cmd/agent-mail
FROM alpine:3.22
RUN addgroup -S agentmail && adduser -S -G agentmail agentmail && mkdir -p /data && chown agentmail:agentmail /data
COPY --from=build /agent-mail /usr/local/bin/agent-mail
USER agentmail
ENV DB_PATH=/data/mail.db LISTEN_ADDR=:8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD wget -q -O - http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["agent-mail"]
