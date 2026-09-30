package repository

import (
	"fmt"
	"strings"

	entsql "entgo.io/ent/dialect/sql"
)

// Search only the account name and public routing/model configuration, never
// the whole credentials object. STRPOS keeps %, _ and backslashes literal.
func accountSearchCondition(nameColumn, credentialsColumn, keyword string) string {
	return fmt.Sprintf(`(
		STRPOS(LOWER(%[1]s), LOWER(%[3]s)) > 0
		OR STRPOS(LOWER(%[2]s->>'base_url'), LOWER(%[3]s)) > 0
		OR EXISTS (
			SELECT 1 FROM jsonb_each(CASE
				WHEN jsonb_typeof(%[2]s->'model_mapping') = 'object' THEN %[2]s->'model_mapping'
				ELSE '{}'::jsonb END) AS model_mapping
			WHERE STRPOS(LOWER(model_mapping.key), LOWER(%[3]s)) > 0
				OR (jsonb_typeof(model_mapping.value) = 'string'
					AND STRPOS(LOWER(model_mapping.value #>> '{}'), LOWER(%[3]s)) > 0)
		)
		OR EXISTS (
			SELECT 1 FROM jsonb_array_elements(CASE
				WHEN jsonb_typeof(%[2]s->'model_whitelist') = 'array' THEN %[2]s->'model_whitelist'
				ELSE '[]'::jsonb END) AS model_name
			WHERE jsonb_typeof(model_name.value) = 'string'
				AND STRPOS(LOWER(model_name.value #>> '{}'), LOWER(%[3]s)) > 0
		)
	)`, nameColumn, credentialsColumn, keyword)
}

func accountSearchPredicate(nameColumn, credentialsColumn, search string) *entsql.Predicate {
	parts := strings.Split(accountSearchCondition(nameColumn, credentialsColumn, "?"), "?")
	return entsql.P(func(b *entsql.Builder) {
		// Every whitespace-separated term must match a public field. Terms may
		// match different fields, and punctuation (including '-') stays literal.
		for i, term := range strings.Fields(search) {
			if i > 0 {
				b.WriteString(" AND ")
			}
			for j, part := range parts {
				if j > 0 {
					b.Arg(term)
				}
				b.WriteString(part)
			}
		}
	})
}

func appendAccountSearchCondition(conditions []string, args []any, accountColumn, search string) ([]string, []any) {
	terms := strings.Fields(search)
	if len(terms) == 0 {
		return conditions, args
	}
	// Historical usage/error rows may refer to a soft-deleted account.
	termConditions := make([]string, 0, len(terms))
	for _, term := range terms {
		termConditions = append(termConditions, accountSearchCondition("search_account.name", "search_account.credentials", fmt.Sprintf("$%d", len(args)+1)))
		args = append(args, term)
	}
	conditions = append(conditions, accountColumn+" IN (SELECT search_account.id FROM accounts AS search_account WHERE "+strings.Join(termConditions, " AND ")+")")
	return conditions, args
}

func appendAccountSearchQueryFilter(query string, args []any, accountColumn, search string) (string, []any) {
	conditions, args := appendAccountSearchCondition(nil, args, accountColumn, search)
	if len(conditions) > 0 {
		query += " AND " + conditions[0]
	}
	return query, args
}
