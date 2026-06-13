package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
)

const selectBusinessCols = `SELECT id, name, category, schema_type, description, city,
	street, locality, postal_code, country, lat, lng,
	price_range, price_from, price_to, price_currency,
	rating, review_count, phone, email, payment,
	image_url, logo_url, images, social_links, opening_hours, reviews,
	sponsored, verified, sources, last_verified`

const listingCols = `source, source_id, url, name, category, schema_type, description,
	city, street, locality, postal_code, country, lat, lng, price_range,
	rating, review_count, phone, email, payment, image_url, logo_url, images,
	social_links, opening_hours, services, reviews, scraped_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanBusiness(row rowScanner) (*domain.Business, error) {
	var b domain.Business
	var lat, lng, rating, priceFrom, priceTo sql.NullFloat64
	var reviewCount, sponsored, verified sql.NullInt64
	var imagesJSON, socialJSON, hoursJSON, reviewsJSON, sourcesJSON, lastVerified string
	err := row.Scan(&b.ID, &b.Name, &b.Category, &b.SchemaType, &b.Description, &b.City,
		&b.Address.Street, &b.Address.Locality, &b.Address.PostalCode, &b.Address.Country,
		&lat, &lng, &b.PriceRange, &priceFrom, &priceTo, &b.PriceCurrency,
		&rating, &reviewCount, &b.Phone, &b.Email, &b.Payment,
		&b.ImageURL, &b.LogoURL, &imagesJSON, &socialJSON, &hoursJSON, &reviewsJSON,
		&sponsored, &verified, &sourcesJSON, &lastVerified)
	if err != nil {
		return nil, err
	}
	b.Sponsored = sponsored.Int64 != 0
	b.Verified = verified.Int64 != 0
	if lat.Valid && lng.Valid {
		b.Latitude, b.Longitude = &lat.Float64, &lng.Float64
	}
	if priceFrom.Valid {
		b.PriceFrom = &priceFrom.Float64
	}
	if priceTo.Valid {
		b.PriceTo = &priceTo.Float64
	}
	if rating.Valid {
		b.Rating = &domain.Rating{Value: rating.Float64, ReviewCount: int(reviewCount.Int64)}
	}
	if err := decodeJSONCols(map[string]jsonCol{
		"images":        {imagesJSON, &b.Images},
		"social links":  {socialJSON, &b.SocialLinks},
		"opening hours": {hoursJSON, &b.OpeningHours},
		"reviews":       {reviewsJSON, &b.Reviews},
		"sources":       {sourcesJSON, &b.Sources},
	}); err != nil {
		return nil, fmt.Errorf("business %d: %w", b.ID, err)
	}
	if t, err := time.Parse(time.RFC3339, lastVerified); err == nil {
		b.LastVerified = t
	}
	// "Unknown": we only know this business from an external dataset
	// (Supabase/Google Maps) — we never scraped it ourselves.
	b.Unknown = len(b.Sources) == 1 && b.Sources[0] == "supabase"
	return &b, nil
}

func scanBusinesses(rows *sql.Rows) ([]domain.Business, error) {
	defer func() { _ = rows.Close() }()
	var out []domain.Business
	for rows.Next() {
		b, err := scanBusiness(rows)
		if err != nil {
			return nil, fmt.Errorf("scan business: %w", err)
		}
		out = append(out, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate businesses: %w", err)
	}
	return out, nil
}

func scanListings(rows *sql.Rows) ([]domain.Listing, error) {
	defer func() { _ = rows.Close() }()
	var out []domain.Listing
	for rows.Next() {
		var l domain.Listing
		var lat, lng, rating sql.NullFloat64
		var reviewCount sql.NullInt64
		var imagesJSON, socialJSON, hoursJSON, servicesJSON, reviewsJSON, scrapedAt string
		err := rows.Scan(&l.Source, &l.SourceID, &l.URL, &l.Name, &l.Category,
			&l.SchemaType, &l.Description, &l.City,
			&l.Address.Street, &l.Address.Locality, &l.Address.PostalCode, &l.Address.Country,
			&lat, &lng, &l.PriceRange, &rating, &reviewCount,
			&l.Phone, &l.Email, &l.Payment, &l.ImageURL, &l.LogoURL, &imagesJSON,
			&socialJSON, &hoursJSON, &servicesJSON, &reviewsJSON, &scrapedAt)
		if err != nil {
			return nil, fmt.Errorf("scan listing: %w", err)
		}
		if lat.Valid && lng.Valid {
			l.Latitude, l.Longitude = &lat.Float64, &lng.Float64
		}
		if rating.Valid {
			l.Rating = &domain.Rating{Value: rating.Float64, ReviewCount: int(reviewCount.Int64)}
		}
		if err := decodeJSONCols(map[string]jsonCol{
			"images":        {imagesJSON, &l.Images},
			"social links":  {socialJSON, &l.SocialLinks},
			"opening hours": {hoursJSON, &l.OpeningHours},
			"services":      {servicesJSON, &l.Services},
			"reviews":       {reviewsJSON, &l.Reviews},
		}); err != nil {
			return nil, fmt.Errorf("listing %s/%s: %w", l.Source, l.SourceID, err)
		}
		if t, err := time.Parse(time.RFC3339, scrapedAt); err == nil {
			l.ScrapedAt = t
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate listings: %w", err)
	}
	return out, nil
}

func scanServices(rows *sql.Rows) ([]domain.ServiceOffer, error) {
	defer func() { _ = rows.Close() }()
	var out []domain.ServiceOffer
	for rows.Next() {
		var svc domain.ServiceOffer
		var price, priceMin, priceMax sql.NullFloat64
		var duration sql.NullInt64
		if err := rows.Scan(&svc.Name, &svc.Description, &price, &priceMin, &priceMax,
			&svc.Currency, &duration, &svc.ImageURL); err != nil {
			return nil, fmt.Errorf("scan service: %w", err)
		}
		if price.Valid {
			svc.Price = &price.Float64
		}
		if priceMin.Valid {
			svc.PriceMin = &priceMin.Float64
		}
		if priceMax.Valid {
			svc.PriceMax = &priceMax.Float64
		}
		if duration.Valid {
			d := int(duration.Int64)
			svc.DurationMin = &d
		}
		out = append(out, svc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate services: %w", err)
	}
	return out, nil
}

// jsonCol pairs a JSON-encoded column value with its decode target.
type jsonCol struct {
	src string
	dst any
}

func decodeJSONCols(cols map[string]jsonCol) error {
	for name, c := range cols {
		if err := json.Unmarshal([]byte(c.src), c.dst); err != nil {
			return fmt.Errorf("decode %s: %w", name, err)
		}
	}
	return nil
}

// listingJSONCols holds the JSON-encoded column values for one listing.
type listingJSONCols struct {
	social, hours, services, images, reviews string
}

func marshalListingJSON(l *domain.Listing) (listingJSONCols, error) {
	var out listingJSONCols
	for _, field := range []struct {
		name string
		v    any
		dst  *string
	}{
		{"social links", l.SocialLinks, &out.social},
		{"opening hours", l.OpeningHours, &out.hours},
		{"services", l.Services, &out.services},
		{"images", l.Images, &out.images},
		{"reviews", l.Reviews, &out.reviews},
	} {
		b, err := json.Marshal(field.v)
		if err != nil {
			return out, fmt.Errorf("marshal %s: %w", field.name, err)
		}
		*field.dst = string(b)
	}
	return out, nil
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
