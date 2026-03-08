package config

import (
	"os"
	"strconv"
)

type Config struct {
	Port        string
	DBPath      string
	WorkerCount int
}

func Load() Config {
	c := Config{
		Port:        "8080",
		DBPath:      "./miniflow.db",
		WorkerCount: 4,
	}
	if v := os.Getenv("PORT"); v != "" {
		c.Port = v
	}
	if v := os.Getenv("DB_PATH"); v != "" {
		c.DBPath = v
	}
	if v := os.Getenv("WORKER_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.WorkerCount = n
		}
	}
	return c
}
