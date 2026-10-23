-- Pi delivery was removed. Its attempts are evidence for a delivery channel
-- that no longer exists, so they are dropped rather than archived. This is a
-- one-way migration: a downgraded binary refuses the newer schema.
DROP TABLE pi_deliveries;
