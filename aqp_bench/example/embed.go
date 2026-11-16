package example

import "embed"

// FS contains the unchanged trusted offline example campaign and evaluator.
//
//go:embed campaign.json numbers.csv check.py
var FS embed.FS
