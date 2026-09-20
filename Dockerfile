
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache sqlite
WORKDIR /app
COPY --from=build /out/server /app/server
COPY migrations /app/migrations
ENV PORT=8080 DATABASE_PATH=/data/app.sqlite3
EXPOSE 8080
# 迁移已内嵌进服务进程，启动时自动应用；这里只需确保数据目录存在。
CMD ["/bin/sh", "-c", "mkdir -p /data && exec /app/server"]
