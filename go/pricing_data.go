package main

// embeddedPricingJSON is populated at release build time via:
//
//	go generate ./...
//
// For local development, pricing.json is loaded from the filesystem.
// The release workflow generates this file with the actual pricing data.
var embeddedPricingJSON = ""
