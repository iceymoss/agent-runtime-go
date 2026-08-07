package config

type Config struct{ Timeout int }

func Default() Config { return Config{Timeout: 0} }
