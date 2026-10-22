package sub

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alireza0/s-ui/database"
	"github.com/alireza0/s-ui/database/model"
	"github.com/alireza0/s-ui/service"
)

// QueryService resolves a client by a credential (uuid or password) taken from
// the client's protocol config, and reports its expiry / traffic / usage. It is
// the backend for the public query endpoint on the subscription port.
type QueryService struct {
	service.SettingService
}

// QueryResult is the payload returned by the query endpoint.
type QueryResult struct {
	Name             string `json:"name"`
	Expiry           int64  `json:"expiry"`
	ExpiryStr        string `json:"expiry_str"`
	UnlimitedTime    bool   `json:"unlimited_time"`
	Volume           int64  `json:"volume"`
	VolumeStr        string `json:"volume_str"`
	UnlimitedTraffic bool   `json:"unlimited_traffic"`
	Used             int64  `json:"used"`
	UsedStr          string `json:"used_str"`
	Up               int64  `json:"up"`
	Down             int64  `json:"down"`
	Devs             int    `json:"devs"`
	Email            string `json:"email"`
	Enable           bool   `json:"enable"`
}

// QueryResponse wraps the result in a uniform envelope.
type QueryResponse struct {
	Code int          `json:"code"`
	Msg  string       `json:"msg"`
	Data *QueryResult `json:"data,omitempty"`
}

// Query looks up a client whose config carries the given uuid or password and
// returns its status. key is matched case-sensitively against every uuid and
// password entry across all clients, so it covers VLESS/VMess/TUIC (uuid) and
// Shadowsocks/Trojan/AnyTLS/Hysteria (password) alike.
func (s *QueryService) Query(key string) *QueryResponse {
	key = strings.TrimSpace(key)
	if key == "" {
		return &QueryResponse{Code: 400, Msg: "key is required"}
	}

	db := database.GetDB()
	var clients []model.Client
	if err := db.Model(model.Client{}).Find(&clients).Error; err != nil {
		return &QueryResponse{Code: 500, Msg: "internal error"}
	}

	for i := range clients {
		if configMatches(&clients[i], key) {
			return &QueryResponse{Code: 200, Msg: "ok", Data: s.buildResult(&clients[i])}
		}
	}
	return &QueryResponse{Code: 404, Msg: "not found"}
}

// parseConfig parses the client's config JSON into a map of protocol entries.
func parseConfig(client *model.Client) (map[string]map[string]interface{}, error) {
	if len(client.Config) < 2 {
		return nil, fmt.Errorf("empty config")
	}
	var configs map[string]map[string]interface{}
	if err := json.Unmarshal(client.Config, &configs); err != nil {
		return nil, err
	}
	return configs, nil
}

// configMatches reports whether any protocol entry in the client's config has a
// uuid or password equal to key.
func configMatches(client *model.Client, key string) bool {
	configs, err := parseConfig(client)
	if err != nil {
		return false
	}
	return configMatchesParsed(configs, key)
}

// configMatchesParsed checks an already-parsed config for a uuid/password match.
func configMatchesParsed(configs map[string]map[string]interface{}, key string) bool {
	for _, cfg := range configs {
		if v, ok := cfg["uuid"].(string); ok && v == key {
			return true
		}
		if v, ok := cfg["password"].(string); ok && v == key {
			return true
		}
	}
	return false
}

func (s *QueryService) buildResult(client *model.Client) *QueryResult {
	used := client.Up + client.Down
	loc, err := s.SettingService.GetTimeLocation()
	if err != nil {
		loc = time.Local
	}

	expiryStr := ""
	if client.Expiry > 0 {
		expiryStr = time.Unix(client.Expiry, 0).In(loc).Format("2006-01-02 15:04:05")
	}

	return &QueryResult{
		Name:             client.Name,
		Expiry:           client.Expiry,
		ExpiryStr:        expiryStr,
		UnlimitedTime:    client.Expiry <= 0,
		Volume:           client.Volume,
		VolumeStr:        formatQueryTraffic(client.Volume),
		UnlimitedTraffic: client.Volume <= 0,
		Used:             used,
		UsedStr:          formatQueryTraffic(used),
		Up:               client.Up,
		Down:             client.Down,
		Devs:             client.Devs,
		Email:            client.Email,
		Enable:           client.Enable,
	}
}

// formatQueryTraffic renders a byte count as a human-readable size.
func formatQueryTraffic(trafficBytes int64) string {
	switch {
	case trafficBytes < 1024:
		return fmt.Sprintf("%.2fB", float64(trafficBytes))
	case trafficBytes < 1024*1024:
		return fmt.Sprintf("%.2fKB", float64(trafficBytes)/1024)
	case trafficBytes < 1024*1024*1024:
		return fmt.Sprintf("%.2fMB", float64(trafficBytes)/(1024*1024))
	case trafficBytes < 1024*1024*1024*1024:
		return fmt.Sprintf("%.2fGB", float64(trafficBytes)/(1024*1024*1024))
	case trafficBytes < 1024*1024*1024*1024*1024:
		return fmt.Sprintf("%.2fTB", float64(trafficBytes)/(1024*1024*1024*1024))
	default:
		return fmt.Sprintf("%.2fEB", float64(trafficBytes)/(1024*1024*1024*1024*1024))
	}
}
