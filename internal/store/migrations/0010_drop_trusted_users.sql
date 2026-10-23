-- Trusted users were removed: every member of the authenticated workspace may
-- send the bot work. The set is dropped rather than archived. This is a
-- one-way migration: a downgraded binary refuses the newer schema.
DROP TABLE trusted_users;
