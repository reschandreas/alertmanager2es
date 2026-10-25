#############################################
# Build
#############################################
FROM golang:1.27.3-alpine AS build

RUN apk upgrade --no-cache --force
RUN apk add --update build-base make git

WORKDIR /go/src/github.com/webdevops/alertmanager2es

# Dependencies
COPY go.mod go.sum ./
RUN go mod download

# Compile
COPY . .
RUN make test
RUN CGO_ENABLED=0 make build

#############################################
# Test
#############################################
FROM scratch AS test
WORKDIR /app
COPY --from=build /go/src/github.com/webdevops/alertmanager2es/alertmanager2es .
RUN ["./alertmanager2es", "--help"]

#############################################
# Final
#############################################
FROM scratch
ENV LOG_JSON=1
WORKDIR /
COPY --from=test /app .
USER 1000:1000
ENTRYPOINT ["/alertmanager2es"]
