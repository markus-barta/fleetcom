# Admin Guide

## User Management

### Creating users

Settings > Users tab > fill email, password (min 6 chars), role > "Add User".

Roles:
- **admin** — full access to all hosts, user management, branding, tokens
- **user** — access only to assigned hosts, self-service account management

New users must set up TOTP on their first login. There is no way to skip this.

### Disabling / deleting users

Click "Disable" to deactivate — all their sessions are killed immediately, but the account can be re-enabled. Click "Disable" again and change status to "deleted" for soft-delete (hidden from UI, data preserved).

You cannot disable or delete your own account.

### Resetting a user's TOTP

If a user loses their authenticator, click "Reset 2FA" in the Users tab. This clears their TOTP secret. On their next login, they'll be forced through TOTP setup again.

### Killing sessions

Click "Kill Sessions" to invalidate all of a user's active sessions. They'll be logged out everywhere.

## Host Permissions

### Granting access

Settings > Users > click "Hosts" next to a user. You'll see two columns:
- **Granted Hosts** — hosts this user can see (with "Revoke" button)
- **Available Hosts** — all other hosts (with "Grant" button)

### How permissions work

- **Admins** always see all hosts — the permission system doesn't apply to them.
- **Regular users** see only hosts they've been explicitly granted. This affects:
  - The dashboard host grid
  - SSE real-time updates (filtered server-side, not just hidden in UI)
  - Host configs, history, and all host-related API endpoints
- **New users** have zero host access by default.

## Branding

### Instance label

Settings > Config > Branding > "Instance Label". Shows in the header next to "FleetCom" (e.g., "BYTEPOETS"). Also sets the domain display. Can also be set via `FLEETCOM_INSTANCE_LABEL` env var.

### Org logo

Settings > Config > Branding > upload an image. Shows in the header to the left of "FleetCom". Click "Remove" to clear it. Stored in the database, not the filesystem.

## Theme

Click the moon/sun icon in the header to cycle: dark → light → auto. Auto follows OS preference. Stored in localStorage per browser.

## Icon Presets

### Uploading

Settings > Icons > "Upload graphic". Transparent PNGs work best. Then assign to hosts in Settings > Hosts > "Configure" per host.

### Export / Import

- **Export**: Settings > Icons > "Export ZIP" — downloads all icons as a ZIP bundle
- **Import**: Settings > Icons > "Import ZIP" — select a ZIP file, optionally check "Overwrite existing icons with the same name", click "Import"

Use this to transfer icons between instances (e.g., personal → company).

## Share Links

Settings > Sharing > create a link with optional label and expiry. Share links give read-only dashboard access without authentication. Delete to revoke.

## Host Tokens

Settings > Hosts > "Add Host" generates a bearer token for the bosun agent. The token is shown once — copy it immediately. Delete a host to revoke its token.

Token management is admin-only.

## Password Reset

If SMTP is configured (`SMTP_HOST` env var), users can reset their password via the "Forgot password?" link on the login page. An email with a reset link (60min TTL, single-use) is sent.

If SMTP is not configured, the reset link is logged to the container's stdout (useful for dev/testing).

All sessions are invalidated when a password is reset.
