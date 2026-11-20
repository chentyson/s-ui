package util

// zhdata inbound helpers (docs/zhdata-admin-api.md §2/§3).
//
// The panel stores a zhdata inbound's port pool the way the frontend sends
// it: `listen_ports` (numbers, the range already expanded) plus `listen`
// holding only the bind host part ("" = all interfaces). The zhdata server
// itself takes a single comma-separated listen string of `host:port` items;
// building that string is the panel's job.

import (
	"fmt"
	"strings"
)

// zhdataListenHost returns the bind host part of a zhdata inbound ("" when
// absent or empty, meaning every interface).
func zhdataListenHost(inbound map[string]interface{}) string {
	host, _ := inbound["listen"].(string)
	return host
}

// zhdataListenPorts returns the port pool of a zhdata inbound.
func zhdataListenPorts(inbound map[string]interface{}) []float64 {
	var ports []float64
	switch raw := inbound["listen_ports"].(type) {
	case []interface{}:
		for _, item := range raw {
			if port, ok := item.(float64); ok && port > 0 {
				ports = append(ports, port)
			}
		}
	case []float64:
		for _, port := range raw {
			if port > 0 {
				ports = append(ports, port)
			}
		}
	}
	return ports
}

// BuildZhdataListen assembles the zhdata server listen semantics:
// "host:port" per entry of listen_ports, joined by ",", with the host part
// empty for "all interfaces" -- ":49888,:49889". Returns "" when the inbound
// names no ports.
func BuildZhdataListen(inbound map[string]interface{}) string {
	ports := zhdataListenPorts(inbound)
	if len(ports) == 0 {
		return ""
	}
	host := zhdataListenHost(inbound)
	items := make([]string, 0, len(ports))
	for _, port := range ports {
		items = append(items, host+":"+fmt.Sprintf("%.0f", port))
	}
	return strings.Join(items, ",")
}
