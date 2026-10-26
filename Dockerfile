FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /inbox-relay .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /inbox-relay /inbox-relay
ENV IR_DATA=/data IR_DEST=/inbox
EXPOSE 8080
ENTRYPOINT ["/inbox-relay"]
CMD ["serve"]
