package store

import "database/sql"

// CollectRows scans all rows into a slice while preserving scan and iteration
// errors supplied by the caller.
func CollectRows[T any](
	rows *sql.Rows,
	capacity int,
	scan func() (T, error),
	wrapRowsError func(error) error,
) ([]T, error) {
	defer rows.Close()
	items := make([]T, 0, capacity)
	for rows.Next() {
		item, err := scan()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		if wrapRowsError != nil {
			return nil, wrapRowsError(err)
		}
		return nil, err
	}
	return items, nil
}
