package geo

import "context"

// ProviderResult is the vendor-neutral result returned by an external GeoIP
// provider. Every value is optional because providers expose different data.
type ProviderResult struct {
	CountryCode *string
	SubLocation *string
	ASN         *string
	ASNName     *string
}

// Provider is the extension point for optional external GeoIP services.
// The open-source core does not know about API keys, endpoints, or vendor
// response formats.
type Provider interface {
	Lookup(ctx context.Context, ip string) (ProviderResult, error)
}
