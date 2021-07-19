package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"

	cowsay "github.com/Code-Hex/Neo-cowsay"
	"github.com/golang/glog"
	"github.com/gomodule/redigo/redis"
	"github.com/gorilla/mux"
	"github.com/kelseyhightower/envconfig"

	_ "github.com/jackc/pgx/v4/stdlib"
)

const (
	CacheHitHeader = "x-cow-cache-hit"
)

type SQLConn struct {
	Database string `envconfig:"DB_NAME"`
	User     string `envconfig:"DB_USER"`
	Password string `envconfig:"DB_PASS"`
	Socket   string `envconfig:"DB_SOCKET"`
}

type RedisConn struct {
	Host string `envconfig:"REDIS_HOST"`
	Port int    `envconfig:"REDIS_PORT"`
}

func main() {
	var sqlconn SQLConn
	if err := envconfig.Process("", &sqlconn); err != nil {
		glog.Exit(err)
	}
	var redisconn RedisConn
	if err := envconfig.Process("", &redisconn); err != nil {
		glog.Exit(err)
	}

	db, err := initDB(sqlconn)
	if err != nil {
		glog.Exit(err)
	}

	redispool, err := initRedis(redisconn)
	if err != nil {
		glog.Exit(err)
	}

	wcs := &whatcowsay{
		db:        db,
		redispool: redispool,
	}
	wcs.setupRoutes()

	glog.Info("Listening on port 8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		glog.Exit(err)
	}
}

type whatcowsay struct {
	db        *sql.DB
	redispool *redis.Pool
	r         *mux.Router
}

func (wcs *whatcowsay) setupRoutes() {
	wcs.r = mux.NewRouter()
	cowPath := wcs.r.Path("/cows/{cow}").Subrouter()
	cowPath.Methods(http.MethodPost).HandlerFunc(wcs.handleCowUpsert)
	cowPath.Methods(http.MethodGet).HandlerFunc(wcs.handleCowSay)
	wcs.r.HandleFunc("/admin", wcs.handleAdmin)
	http.Handle("/", wcs.r)
}

func (wcs *whatcowsay) handleCowUpsert(w http.ResponseWriter, req *http.Request) {
	cow := mux.Vars(req)["cow"]

	if wcs.redispool == nil && wcs.db == nil {
		http.Error(w, "No persistent store available to store what the cow is going to say", http.StatusInternalServerError)
		return
	}

	b, err := ioutil.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "Failed to read HTTP request body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	say, err := saySomething(string(b))
	if err != nil {
		http.Error(w, "Failed to generate figure: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if wcs.db == nil {
		glog.Warningf("DB is not used; will save the figure %q to cache", cow)
		if err := wcs.setRedisVal(cow, say); err != nil {
			http.Error(w, "Failed to save to cache: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		return
	}

	if err := wcs.passThroughSaveVal(cow, say); err != nil {
		http.Error(w, "Failed to save to db: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (wcs *whatcowsay) handleCowSay(w http.ResponseWriter, req *http.Request) {
	cow := mux.Vars(req)["cow"]

	if wcs.redispool == nil && wcs.db == nil {
		t, err := saySomething(fmt.Sprintf("Hi, I'm %s. Temporary. Don't count on me~", cow))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(t))
		return
	}

	if wcs.redispool != nil {
		t, err := wcs.getRedisVal(cow)
		// If we can get the value from the cache.
		if err == nil && t != "" {
			w.Header().Set(CacheHitHeader, "true")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(t))
			return
		}
		if err != nil {
			glog.Warningf("Redis error: %v", err)
		}
	}

	t, err := wcs.passThroughGetVal(cow)
	if err == sql.ErrNoRows {
		http.Error(w, "No figure found!", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "DB error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(t))
}

func (wcs *whatcowsay) getRedisVal(key string) (string, error) {
	if wcs.redispool == nil {
		return "", errors.New("Redis is not used")
	}
	c := wcs.redispool.Get()
	defer c.Close()
	c.Send("GET", key)
	c.Flush()
	return redis.String(c.Receive())
}

func (wcs *whatcowsay) setRedisVal(key, val string) error {
	if wcs.redispool == nil {
		return errors.New("Redis is not used")
	}
	c := wcs.redispool.Get()
	defer c.Close()
	c.Send("SET", key, val)
	c.Send("EXPIRE", key, "20")
	c.Flush()
	if _, err := c.Receive(); err != nil {
		return err
	}
	return nil
}

func (wcs *whatcowsay) passThroughSaveVal(key, val string) error {
	q := `INSERT INTO cows (name, message)
VALUES ($1, $2)
ON CONFLICT (name)
DO UPDATE SET message = $2;`

	if _, err := wcs.db.Exec(q, key, val); err != nil {
		return err
	}
	if err := wcs.setRedisVal(key, val); err != nil {
		glog.Warningf("Failed to pass-through save figure %q in cache", key)
	}
	return nil
}

func (wcs *whatcowsay) passThroughGetVal(key string) (string, error) {
	var text string
	err := wcs.db.QueryRow("SELECT message FROM cows WHERE name = $1", key).Scan(&text)
	if err != nil {
		return "", err
	}
	if err := wcs.setRedisVal(key, text); err != nil {
		glog.Warningf("Failed to pass-through save figure %q in cache", key)
	}
	return text, nil
}

func (wcs *whatcowsay) handleAdmin(w http.ResponseWriter, req *http.Request) {
	http.Error(w, "not implemented", http.StatusNotAcceptable)
}

func saySomething(text string) (string, error) {
	return cowsay.Say(
		cowsay.Phrase(text),
		cowsay.BallonWidth(40),
		cowsay.Type(pickCow()),
	)
}

// To censor the original inappropreiate figures -_-.
func pickCow() string {
	allowed := []string{
		"cows/beavis.zen.cow",
		"cows/bud-frogs.cow",
		"cows/bunny.cow",
		"cows/daemon.cow",
		"cows/default.cow",
		"cows/docker.cow",
		"cows/dragon.cow",
		"cows/elephant.cow",
		"cows/flaming-sheep.cow",
		"cows/ghostbusters.cow",
		"cows/gopher.cow",
		"cows/hellokitty.cow",
		"cows/kitty.cow",
		"cows/koala.cow",
		"cows/meow.cow",
		"cows/sage.cow",
		"cows/sheep.cow",
		"cows/skeleton.cow",
		"cows/squirrel.cow",
		"cows/stegosaurus.cow",
		"cows/turkey.cow",
		"cows/turtle.cow",
	}
	cand := allowed[rand.Intn(len(allowed))]

	for _, n := range cowsay.AssetNames() {
		if n == cand {
			return cand
		}
	}

	return "cows/default.cow"
}

func initRedis(redisconn RedisConn) (*redis.Pool, error) {
	if redisconn.Host == "" {
		return nil, nil
	}

	redisAddr := fmt.Sprintf("%s:%d", redisconn.Host, redisconn.Port)
	const maxConnections = 20
	return &redis.Pool{
		MaxIdle: maxConnections,
		Dial:    func() (redis.Conn, error) { return redis.Dial("tcp", redisAddr) },
	}, nil
}

func initDB(sqlconn SQLConn) (*sql.DB, error) {
	if sqlconn.Database == "" {
		return nil, nil
	}

	db, err := initSocketConnectionPool(sqlconn)
	if err != nil {
		return nil, err
	}

	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS cows ( name VARCHAR(255) NOT NULL, message TEXT NOT NULL, PRIMARY KEY (name) );`); err != nil {
		return nil, fmt.Errorf("DB.Exec: unable to create table: %w", err)
	}

	return db, nil
}

func initSocketConnectionPool(sqlconn SQLConn) (*sql.DB, error) {
	var dbURI string
	dbURI = fmt.Sprintf("user=%s password=%s database=%s host=%s", sqlconn.User, sqlconn.Password, sqlconn.Database, sqlconn.Socket)

	// dbPool is the pool of database connections.
	dbPool, err := sql.Open("pgx", dbURI)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %v", err)
	}

	configureConnectionPool(dbPool)
	return dbPool, nil
}

func configureConnectionPool(dbPool *sql.DB) {
	dbPool.SetMaxIdleConns(5)
	dbPool.SetMaxOpenConns(7)
	dbPool.SetConnMaxLifetime(1800)
}
