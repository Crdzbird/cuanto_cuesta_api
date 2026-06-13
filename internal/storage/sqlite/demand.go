package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/crdzbird/cuanto_cuesta/internal/domain"
)

// DemandStats implements domain.BusinessRepository. It aggregates review
// engagement for businesses in city whose category is one of categories —
// the real demand signal (search volume itself is modeled upstream).
func (r *Repo) DemandStats(ctx context.Context, city string, categories []string) (domain.Demand, error) {
	d := domain.Demand{City: city, Categories: categories}
	if len(categories) == 0 {
		return d, nil
	}

	placeholders := make([]string, len(categories))
	args := []any{city}
	for i, c := range categories {
		placeholders[i] = "?"
		args = append(args, c)
	}
	in := strings.Join(placeholders, ",")

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, category, rating, review_count
		FROM businesses
		WHERE city = ? AND category IN (`+in+`)`, args...)
	if err != nil {
		return domain.Demand{}, fmt.Errorf("demand businesses: %w", err)
	}
	defer func() { _ = rows.Close() }()

	catCount := map[string]int{}
	catReviews := map[string]int{}
	var ratingSum float64
	var rated int
	for rows.Next() {
		var b domain.DemandBiz
		var rating sql.NullFloat64
		var reviews sql.NullInt64
		if err := rows.Scan(&b.ID, &b.Name, &b.Category, &rating, &reviews); err != nil {
			return domain.Demand{}, fmt.Errorf("scan demand row: %w", err)
		}
		b.Reviews = int(reviews.Int64)
		if rating.Valid {
			b.Rating = rating.Float64
			ratingSum += rating.Float64
			rated++
		}
		d.Businesses++
		d.TotalReviews += b.Reviews
		catCount[b.Category]++
		catReviews[b.Category] += b.Reviews
		d.Top = append(d.Top, b)
	}
	if err := rows.Err(); err != nil {
		return domain.Demand{}, fmt.Errorf("iterate demand: %w", err)
	}
	if rated > 0 {
		d.AvgRating = ratingSum / float64(rated)
	}

	d.ByCategory = facetsFromMap(catCount)
	d.ReviewsByCategory = facetsFromMap(catReviews)
	sort.Slice(d.Top, func(i, j int) bool { return d.Top[i].Reviews > d.Top[j].Reviews })
	if len(d.Top) > 10 {
		d.Top = d.Top[:10]
	}

	// Neighborhood engagement: join externals (which carry the neighborhood)
	// to the in-scope businesses.
	nrows, err := r.db.QueryContext(ctx, `
		SELECT e.neighborhood, COUNT(DISTINCT b.id), COALESCE(SUM(b.review_count),0)
		FROM externals e JOIN businesses b ON b.id = e.business_id
		WHERE b.city = ? AND b.category IN (`+in+`) AND e.neighborhood != ''
		GROUP BY e.neighborhood ORDER BY 3 DESC`, args...)
	if err != nil {
		return domain.Demand{}, fmt.Errorf("demand neighborhoods: %w", err)
	}
	defer func() { _ = nrows.Close() }()
	for nrows.Next() {
		var a domain.DemandArea
		if err := nrows.Scan(&a.Name, &a.Businesses, &a.Reviews); err != nil {
			return domain.Demand{}, fmt.Errorf("scan neighborhood: %w", err)
		}
		d.Neighborhoods = append(d.Neighborhoods, a)
	}
	if err := nrows.Err(); err != nil {
		return domain.Demand{}, fmt.Errorf("iterate neighborhoods: %w", err)
	}
	return d, nil
}

func facetsFromMap(m map[string]int) []domain.Facet {
	out := make([]domain.Facet, 0, len(m))
	for k, v := range m {
		out = append(out, domain.Facet{Value: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	return out
}
