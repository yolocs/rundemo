# rundemo

## Requests

To save a figure, send a `POST` request to your running service something to say.

```bash
curl -d "Hello World" -X POST https://[your_domain]/cows/[figure_name]
```

To retrieve a figure, send a `GET` request.

```bash
curl -i https://[your_domain]/cows/[figure_name]
```

## No storage

When no env var is provided, the app won't connect to any storage. The `POST` request will fail and the `GET` request will always return a temporary figure.

## With Redis

Provide the following env vars to configure a Redis connection

```go
type RedisConn struct {
	Host string `envconfig:"REDIS_HOST"`
	Port int    `envconfig:"REDIS_PORT"`
}
```

The `POST` request will save the figure in the Redis instance with 20s expiration time. With that 20s, `GET` requests will get what you saved. After that, it will return `404`. If the value is retrieved from the Redis, the response header will have `x-cow-cache-hit=true`.

## With SQL

Provide the following env vars **in addition to** the Redis env vars

```go
type SQLConn struct {
	Database string `envconfig:"DB_NAME"`
	User     string `envconfig:"DB_USER"`
	Password string `envconfig:"DB_PASS"`
	Socket   string `envconfig:"DB_SOCKET"`
}
```

The `POST` request will cache the figure in the Redis instance with 20s expiration time **and** persist the figure in the DB. `GET` requests will get what you saved. The Redis will work as a pass-through cache. If it's a cache hit, the response header will have `x-cow-cache-hit=true`.
