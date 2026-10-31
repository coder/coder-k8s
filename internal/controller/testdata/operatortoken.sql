CREATE TYPE login_type AS ENUM ('none', 'token');
CREATE TYPE user_status AS ENUM ('active');
CREATE TYPE api_key_scope AS ENUM ('coder:all');
CREATE TABLE users (
  id uuid PRIMARY KEY, email text, username text, name text,
  hashed_password bytea, created_at timestamptz, updated_at timestamptz,
  rbac_roles text[], login_type login_type, status user_status,
  is_system boolean, deleted boolean NOT NULL DEFAULT false
);
CREATE TABLE organizations (id uuid PRIMARY KEY, deleted boolean NOT NULL DEFAULT false);
INSERT INTO organizations (id) VALUES ('00000000-0000-0000-0000-000000000001');
CREATE TABLE organization_members (
  organization_id uuid, user_id uuid, created_at timestamptz,
  updated_at timestamptz, roles text[], PRIMARY KEY (organization_id, user_id)
);
CREATE TABLE api_keys (
  id text PRIMARY KEY, hashed_secret bytea, user_id uuid,
  last_used timestamptz, expires_at timestamptz, created_at timestamptz,
  updated_at timestamptz, login_type login_type, lifetime_seconds bigint,
  ip_address inet, token_name text, scopes api_key_scope[], allow_list text[]
);
CREATE TABLE deleted_keys (token_id text, token_name text);
CREATE FUNCTION record_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO deleted_keys VALUES (OLD.id, OLD.token_name);
  RETURN OLD;
END
$$;
CREATE TRIGGER api_keys_deleted AFTER DELETE ON api_keys FOR EACH ROW EXECUTE FUNCTION record_delete();
