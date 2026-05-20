package main

import "strings"

func parseScannerPresetLines() []string {
	var lines []string
	for _, line := range strings.Split(defaultScannerPresets, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines
}

func parseScannerPresetCount() int {
	return len(parseScannerPresetLines())
}
