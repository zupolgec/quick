package main

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

// handleConfig publicly exposes what the CLI needs to self-configure: OAuth
// client, hosted domain, sites domain, server version. Note the OAuth client
// secret IS served when set: required by Google's token exchange for Web-type
// clients. Use a Desktop-type client for the CLI to keep it out of here.
func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(quick.ConfigResponse{
		OAuthClientID:     s.clientID,
		OAuthClientSecret: s.clientSecret,
		HostedDomain:      s.domain,
		BaseDomain:        s.baseDomain,
		Version:           version,
	})
}

func storageConfigFromEnv() storage.Config {
	return storage.Config{
		Kind:     quick.Env("QUICK_STORAGE", "local"),
		SitesDir: quick.Env("QUICK_SITES_DIR", "./sites"),
		MetaDir:  quick.Env("QUICK_META_DIR", "./meta"),
		S3: storage.S3Config{
			Endpoint:  os.Getenv("QUICK_S3_ENDPOINT"),
			Region:    os.Getenv("QUICK_S3_REGION"),
			Bucket:    os.Getenv("QUICK_S3_BUCKET"),
			AccessKey: os.Getenv("QUICK_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("QUICK_S3_SECRET_KEY"),
			Prefix:    os.Getenv("QUICK_S3_PREFIX"),
			UseSSL:    quick.Env("QUICK_S3_USE_SSL", "true") == "true",
		},
	}
}
