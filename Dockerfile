# Reproducible build of the DarkCode binary. Multi-stage: compile a static
# binary, then ship it on a small image that still has the tools the agent
# shells out to (bash, git, ripgrep, curl).

FROM golang:1.24-alpine AS build
WORKDIR /src
# Cache modules first.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/darkcode .

FROM alpine:3.20
RUN apk add --no-cache bash git ripgrep curl ca-certificates
COPY --from=build /out/darkcode /usr/local/bin/darkcode
# State (config, memory, projects, models) lives under ~/.darkcode — mount a
# volume here to persist it across container restarts.
VOLUME ["/root/.darkcode"]
# GUI mode serves the web UI on 12345 (bind to loopback inside the container;
# publish explicitly with -p if you want to reach it from the host).
EXPOSE 12345
ENTRYPOINT ["darkcode"]
