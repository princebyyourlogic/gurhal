FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/ ./cmd/psud ./cmd/psuctl && mkdir -p /out/state

FROM scratch
COPY --from=build /out/psud /out/psuctl /
# Owned by the runtime user so a fresh named volume mounted here is writable.
COPY --from=build --chown=65534:65534 /out/state /state
COPY LICENSE THIRD_PARTY_NOTICES.md /
USER 65534:65534
# PATH lets `docker compose exec psud psuctl ...` find psuctl (scratch has no shell).
ENV PSU_STATE_DIR=/state PATH=/
EXPOSE 11110 11140 12110 12140 12061
ENTRYPOINT ["/psud"]
