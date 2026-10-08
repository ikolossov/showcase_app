FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server .

FROM scratch
# Корневые сертификаты — для HTTPS-запросов к api.github.com (GITHUB_REPO).
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
