CREATE TABLE trusted_users (
  slack_user_id TEXT PRIMARY KEY CHECK (
    length(slack_user_id) BETWEEN 9 AND 64
    AND substr(slack_user_id, 1, 1) IN ('U', 'W')
    AND slack_user_id NOT GLOB '*[^A-Z0-9]*'
  )
) STRICT;
