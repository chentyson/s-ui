package migration

// Refresh the zhdata share URIs the panel stored in clients.links
// (docs/zhdata-admin-api.md §3.2).
//
// clients.links is only rewritten when a client or one of its inbounds is
// saved, so an upgrade leaves the old URI exactly where it was: the client
// modal, its copy button and the QR code all read that column, and nothing else
// in the panel surfaces it. ZY5 built a zhdata URI whose host part listed the
// inbound's whole listen port pool -- a 64-port inbound became a
// several-hundred-byte link that subscriber apps choke on -- while ZY6 writes
// only the pool's first port, because the zhdata client expands the pool itself
// (slot i dials servers[i%N], and zhenhanvn defaults to 32 slots starting at
// that first port), so one endpoint is all a subscriber needs.
//
// Only zhdata entries are touched: a vless or hysteria link the operator is
// happy with keeps its exact bytes. The zhdata entry is regenerated through the
// same builder a client save uses, so the stored value ends up identical to
// what saving would produce, remark included. The step is idempotent -- a link
// already in the current form is left alone -- because `sui migrate` also runs
// after a database restore.
//
// Migrations do not run by themselves at panel start: this fires on
// `sui migrate` (the upgrade path) and on ImportDB (database/backup.go).

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/alireza0/s-ui/database/model"
	"github.com/alireza0/s-ui/util"

	"gorm.io/gorm"
)

func to1_6_5(tx *gorm.DB) error {
	refreshed, err := refreshZhdataLinks(tx)
	if err != nil {
		return err
	}
	if refreshed > 0 {
		fmt.Printf("zhdata: refreshed the share link of %d client(s)\n", refreshed)
	}
	return nil
}

// linkRow carries only the columns this step reads: the clients table keeps
// growing fields this migration has no business touching.
type linkRow struct {
	Id     uint
	Name   string
	Config []byte
	Links  []byte
}

func refreshZhdataLinks(tx *gorm.DB) (int, error) {
	if !tx.Migrator().HasTable("clients") || !tx.Migrator().HasTable("inbounds") {
		return 0, nil
	}

	var inbounds []model.Inbound
	if err := tx.Preload("Tls").Where("type = ?", "zhdata").Find(&inbounds).Error; err != nil {
		return 0, err
	}
	if len(inbounds) == 0 {
		return 0, nil
	}
	byTag := make(map[string]*model.Inbound, len(inbounds))
	for i := range inbounds {
		byTag[inbounds[i].Tag] = &inbounds[i]
	}

	var clients []linkRow
	if err := tx.Raw("SELECT id, name, config, links FROM clients").Scan(&clients).Error; err != nil {
		return 0, err
	}

	refreshed := 0
	for _, client := range clients {
		var entries []map[string]string
		if len(client.Links) > 0 {
			if err := json.Unmarshal(client.Links, &entries); err != nil {
				fmt.Printf("zhdata: skipping client %q, cannot parse links: %v\n", client.Name, err)
				continue
			}
		}

		changed := false
		for _, entry := range entries {
			if entry["type"] != "local" || !strings.HasPrefix(entry["uri"], "zhdata://") {
				continue
			}
			// A local link is labelled with the tag of the inbound it came
			// from (service.ClientService.updateLinksWithFixedInbounds), which
			// is how a stored URI is traced back to its inbound here.
			inbound, ok := byTag[entry["remark"]]
			if !ok {
				continue
			}
			want, ok := zhdataShareLink(client, inbound, entry["uri"])
			if !ok || want == entry["uri"] {
				continue
			}
			fmt.Printf("zhdata: client %q link refreshed\n  was: %s\n  now: %s\n", client.Name, entry["uri"], want)
			entry["uri"] = want
			changed = true
		}
		if !changed {
			continue
		}

		encoded, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return 0, err
		}
		// Raw bytes, not a string: the column is a BLOB and a TEXT value lands
		// somewhere GORM cannot scan back into json.RawMessage.
		if err := tx.Exec("UPDATE clients SET links = ? WHERE id = ?", encoded, client.Id).Error; err != nil {
			return 0, err
		}
		refreshed++
	}
	return refreshed, nil
}

// zhdataShareLink rebuilds the one zhdata URI this client holds for this
// inbound through the panel's normal link builder, so the result matches a
// client save down to the remark.
//
// A zhdata inbound usually carries no addrs of its own: the panel filled the
// address from the request's Host header when the link was generated, and a
// migration has no request to read. The stored URI names that host -- every
// endpoint in it does, because the pool is one machine's port range -- so it is
// recovered from there instead of leaving the bloated URI standing. The step
// declines (ok=false) when the inbound names several addresses (no telling which
// stored entry is whose), when the stored endpoints span more than one host, or
// when the client holds no zhdata credentials.
func zhdataShareLink(client linkRow, inbound *model.Inbound, stored string) (string, bool) {
	if len(client.Config) == 0 {
		return "", false
	}

	var addrs []map[string]interface{}
	_ = json.Unmarshal(inbound.Addrs, &addrs) // a null column, the usual shape, leaves this empty
	switch len(addrs) {
	case 0:
		host, ok := sharedEndpointHost(stored)
		if !ok {
			return "", false
		}
		// On a copy: the panel fills the address per request, the loaded row
		// stays as loaded.
		withHost := *inbound
		withHost.Addrs, _ = json.Marshal([]map[string]interface{}{
			{"server": host, "server_port": 0, "remark": ""},
		})
		inbound = &withHost
	case 1: // the address is already named
	default:
		return "", false
	}

	links := util.LinkGenerator(client.Config, inbound, "", client.Name)
	var zhdata []string
	for _, link := range links {
		parsed, err := url.Parse(link)
		if err == nil && parsed.Scheme == "zhdata" {
			zhdata = append(zhdata, link)
		}
	}
	if len(zhdata) != 1 {
		return "", false
	}
	return zhdata[0], true
}

// sharedEndpointHost returns the single host a stored zhdata URI names. The old
// builder wrote one endpoint per port of the inbound's pool, so the authority
// part reads "host:port,host:port,...". More than one host means the link spans
// machines, which the port pool alone cannot reconstruct.
func sharedEndpointHost(stored string) (string, bool) {
	rest := strings.TrimPrefix(stored, "zhdata://")
	if i := strings.IndexAny(rest, "/#?"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}

	host := ""
	for _, endpoint := range strings.Split(rest, ",") {
		i := strings.LastIndex(endpoint, ":")
		if i <= 0 {
			return "", false
		}
		name := endpoint[:i]
		if host == "" {
			host = name
		} else if !strings.EqualFold(host, name) {
			return "", false
		}
	}
	if host == "" {
		return "", false
	}
	return host, true
}
