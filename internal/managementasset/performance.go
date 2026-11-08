package managementasset

import (
	"bytes"
	"fmt"
)

const performanceMarker = `id="cpa-performance-script"`

func AddPerformanceToManagementHTML(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty management html")
	}
	if bytes.Contains(data, []byte(performanceMarker)) {
		return data, nil
	}
	index := bytes.LastIndex(bytes.ToLower(data), []byte("</body>"))
	if index < 0 {
		index = len(data)
	}
	return insertBytes(data, index, []byte(`<link rel="stylesheet" href="./management-agent/performance.css"><script id="cpa-performance-script" src="./management-agent/performance.js"></script>`)), nil
}
