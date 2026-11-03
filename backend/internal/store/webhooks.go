package store

import "context"

// RecordDelivery notes a webhook delivery by its ID, reporting whether it's
// new: false means it was handled before (a redelivery) and must be ignored.
// Deliveries older than 30 days are forgotten as new ones arrive; GitHub
// only lets you redeliver recent ones.
func (s *Store) RecordDelivery(ctx context.Context, id string) (isNew bool, err error) {
	if _, err := s.pool.Exec(ctx, "DELETE FROM webhook_deliveries WHERE received_at < now() - interval '30 days'"); err != nil {
		return false, translate(err)
	}
	tag, err := s.pool.Exec(ctx, "INSERT INTO webhook_deliveries (id) VALUES ($1) ON CONFLICT DO NOTHING", id)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() == 1, nil
}

// ForgetDelivery removes a delivery's record, so a redelivery of it is
// handled: for a delivery that failed before it did anything.
func (s *Store) ForgetDelivery(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM webhook_deliveries WHERE id = $1", id)
	return translate(err)
}
