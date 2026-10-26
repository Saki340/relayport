FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /relayport .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /relayport /relayport
ENV RELAYPORT_DATA=/data RELAYPORT_DEST=/inbox
EXPOSE 8080
ENTRYPOINT ["/relayport"]
CMD ["serve"]
