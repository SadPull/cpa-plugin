// region.go owns the dual-realm topology: Qoder international (qoder.sh) and
// Qoder CN (qoder.com.cn) expose the same product over different domains and
// two different inference protocols:
//
//	global — Qoder CLI current protocol: POST {chatBase}/model/v1/chat/
//	         completions is OpenAI-native (Bearer dt-/jt-, no COSY signing,
//	         standard OpenAI SSE). Verified live 2026-08 against api2-v2
//	         .qoder.sh (QoderGateway protocol research §2/§7).
//	cn     — legacy gateway protocol: COSY-signed + QoderEncoding body against
//	         {gatewayBase}/algo/api/v2/service/pro/sse/agent_chat_generation,
//	         double-nested SSE (see sign.go / encoding.go / body.go).
//
// Region is stored per auth file (storedAuth.Auth.Region, with the historical
// Domain field as fallback) and resolved through regionForAuth everywhere.
package main

import (
	"strings"
	"sync"
)

// Region identifiers stored in auth files and plugin config.
const (
	RegionGlobal = "global"
	RegionCN     = "cn"
)

// regionSpec carries every base URL a region needs. Endpoint paths that are
// identical across realms (userinfo, quota, jobToken family, deviceToken
// family, check-in family) are derived from OpenAPIBase at call time; the two
// inference stacks differ structurally, so Chat/Models URLs are explicit.
type regionSpec struct {
	Region      string
	OpenAPIBase string // auth + business endpoints (Bearer, no COSY)
	GatewayBase string // COSY inference gateway (CN); unused for global
	ChatURL     string // full inference endpoint (COSY variant)
	ModelsURL   string // full model-list endpoint (COSY variant)
	ChatAPIBase string // OpenAI-native inference base (global)
	WebsiteBase string // device-authorization pages
	ClientID    string // device-flow client_id
}

var regionSpecs = map[string]regionSpec{
	// International realm. client_id is the Qoder CLI (qoderclicn 1.1.16)
	// device-flow client, verified against openapi.qoder.sh deviceToken/poll.
	RegionGlobal: {
		Region:      RegionGlobal,
		OpenAPIBase: "https://openapi.qoder.sh",
		ChatAPIBase: "https://api2-v2.qoder.sh",
		ChatURL:     "https://api2-v2.qoder.sh/model/v1/chat/completions",
		// Speculative: model/list is only proven on the CN gateway. The plugin
		// falls back to the static list when this 404s/401s (see callModelsAPI).
		ModelsURL:   "https://api2-v2.qoder.sh/algo/api/v2/model/list?Encode=1",
		GatewayBase: "https://api2-v2.qoder.sh",
		WebsiteBase: "https://qoder.com",
		ClientID:    "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
	},
	// CN realm (QoderWork). COSY-signed gateway inference; auth pages behind
	// Aliyun SSO. client_id from the CN desktop client (qoderwork oauth.go).
	RegionCN: {
		Region:      RegionCN,
		OpenAPIBase: "https://openapi.qoder.com.cn",
		GatewayBase: "https://gateway.qoder.com.cn",
		ChatURL:     "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
		ModelsURL:   "https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1",
		WebsiteBase: "https://qoder.com.cn",
		ClientID:    "1c5e33e1-364d-4ce6-b02c-acaa81274a5c",
	},
}

// normalizeRegion maps free-form region/domain strings onto the two known
// regions. Unknown values fall back to def (never error — an unparseable
// region must not break execution).
func normalizeRegion(s, def string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case RegionGlobal, "qoder.sh", "qoder.com", "intl", "international", "国际":
		return RegionGlobal
	case RegionCN, "qoder.com.cn", "china", "国内":
		return RegionCN
	default:
		return def
	}
}

// defaultRegion is the plugin-config default (config key "default_region",
// env QODER_PLUGIN_REGION). Accounts created without an explicit region
// inherit it. Default global: the international protocol is the verified,
// recommended path.
var (
	defaultRegionMu   sync.RWMutex
	defaultRegionVal  = RegionGlobal
	defaultClientIDMu sync.RWMutex
	defaultClientID   = "" // optional config override for the device-flow client_id
)

func defaultRegion() string {
	defaultRegionMu.RLock()
	defer defaultRegionMu.RUnlock()
	return defaultRegionVal
}

func setDefaultRegion(r string) {
	r = normalizeRegion(r, RegionGlobal)
	defaultRegionMu.Lock()
	defaultRegionVal = r
	defaultRegionMu.Unlock()
}

func configClientID() string {
	defaultClientIDMu.RLock()
	defer defaultClientIDMu.RUnlock()
	return defaultClientID
}

func setConfigClientID(id string) {
	defaultClientIDMu.Lock()
	defaultClientID = strings.TrimSpace(id)
	defaultClientIDMu.Unlock()
}

// specFor returns the endpoint table for a region (falls back to default).
func specFor(region string) regionSpec {
	spec, ok := regionSpecs[normalizeRegion(region, defaultRegion())]
	if !ok {
		spec = regionSpecs[defaultRegion()]
	}
	if cid := configClientID(); cid != "" {
		spec.ClientID = cid
	}
	return spec
}

// regionForAuth resolves the region of one stored credential:
//  1. explicit Auth.Region (new field),
//  2. legacy Auth.Domain ("qoder.com.cn" / "qoder.sh" / "qoder.com"),
//  3. plugin default.
func regionForAuth(sa *storedAuth) string {
	if sa != nil {
		if r := strings.TrimSpace(sa.Auth.Region); r != "" {
			return normalizeRegion(r, defaultRegion())
		}
		if d := strings.TrimSpace(sa.Auth.Domain); d != "" {
			return normalizeRegion(d, defaultRegion())
		}
	}
	return defaultRegion()
}

// regionDomain is the canonical Domain value persisted alongside Region so
// legacy tooling (and host_auth.go content classification) keeps working.
func regionDomain(region string) string {
	if normalizeRegion(region, RegionGlobal) == RegionCN {
		return "qoder.com.cn"
	}
	return "qoder.sh"
}

// isQoderRegionDomain reports whether a domain string belongs to either Qoder
// realm. Used by auth-file ownership checks (host_auth.go).
func isQoderRegionDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return false
	}
	for _, suffix := range []string{"qoder.com.cn", "qoder.com", "qoder.sh"} {
		if d == suffix || strings.HasSuffix(d, "."+suffix) {
			return true
		}
	}
	return false
}
