# Odoo commercial intelligence synchronization

This feature is disabled by default. It adds a separate consumer to the existing
Meet process, backed by a SQLite outbox. It never runs ASR again and does not
create CRM opportunities, tasks or mail.

## Contract and behavior

The receiver is the companion `transgenia_meet_intelligence` Odoo 19 addon.
Its native JSON-2 method is
`/json/2/transgenia.meet.session/ingest_snapshot`, with `{"payload": ...}`.

Each payload has `schema_version=1`, `source_id`, decimal `external_id`,
positive `revision` and `generation`, and `operation=upsert|delete`.
The generation changes when SQLite reuses a deleted minutes ID. A receiver must
clear human review and manually linked clients on generation change, even if
the owner or evidence is unchanged. Revisions order changes within that source.

An upsert contains the meeting date, owner, declared client/project, summary,
textual sentiment, commercial signals and analysis/model versions. It never
contains source audio, the full transcript or participant recipient lists.
A deletion contains only the six envelope fields. Cancellation, retention and
room deletion retract the remote content. A queued newer revision wins over
older acknowledgements. Reassigning the owner first retracts the remote record;
an operator must re-enqueue after verifying the new mapping (or a later ready
revision can enqueue it).

The receiver returns only `source_id`, `external_id`, the REQUESTED `revision`
and `status=applied|duplicate|stale`; optional `current_revision` is allowed.
Changed data with the same revision is rejected by the receiver. Retrying an
ambiguous timeout is safe only because the receiver enforces this contract.

Requests require HTTPS, reject redirects and have a 15-second timeout. A lease
protects each delivery; errors are sanitized. After eight failed attempts the
record remains failed for operator review. A changed destination/source binding
is refused instead of replaying existing data elsewhere.

## Commercial analysis

The existing minutes model call also extracts up to twelve evidence-backed
hypotheses: renewal, upsell, cross-sell, new opportunity or retention risk.
Intent is distinct from sentiment: exploration, explicit need, quote request or
objection. Each hypothesis includes a literal quote, transcript speaker label,
reason, suggested next step and qualitative confidence.

No monetary value, probability of purchase or verified speaker identity is
inferred. Positive sentiment alone is not purchase intent. Historical minutes
without this analysis remain `meet-minutes-legacy`; they must not be counted as
analyzed meetings with zero opportunities. Newly analyzed empty arrays mean the
analysis found no supported hypotheses.

The internal host view and Odoo can display these hypotheses. Participant
Markdown, email text/HTML and attachments do not include this internal section.
A human must confirm customer association and review a signal before using the
Odoo action that opens an opportunity form.

## Preparing activation

Set these non-secret settings only after installing and testing the receiver:

- `ODOO_SYNC_ENABLED=true`
- `ODOO_BASE_URL`: verified HTTPS origin of the intended Odoo instance.
- `ODOO_DATABASE`: optional explicit database.
- `ODOO_SOURCE_ID`: stable 8-80 character identifier registered on the receiver.
- `ODOO_API_KEY_FILE`: path to the dedicated integration credential.

The optional `compose.odoo-sync.yaml` adds a file-secret mount. The base Compose
file does not require that secret while synchronization is disabled.
The Odoo source must bind the integration account, company and permitted owners.
There is no implicit lookup or creation of contacts based on conversation text.

### Existing accounts with different profile addresses

If a host's Cloudflare Access address differs from the existing Odoo user's
profile address, configure `ODOO_OWNER_MAP_FILE` before the first activation.
The sender translates only the outbound snapshot's `owner_email`. The host's
Meet identity, room ownership, Cloudflare PIN sign-in and Odoo user/contact
records remain unchanged. This mapping does not create or rename accounts.

The private JSON format is:

```json
{
  "version": 1,
  "odoo_base_url": "https://odoo.example.test",
  "odoo_database": "example-db",
  "odoo_source_id": "meet-example-prod",
  "owners": [
    {
      "meet_email": "host@example.test",
      "odoo_user_id": 101,
      "odoo_email": "existing-profile@example.test"
    }
  ]
}
```

Use the read-only account review to obtain each existing user's ID and profile
address, and verify the receiver's allowed owners against those same IDs. The
receiver still resolves the target address under its own account and company
checks; the ID in this file records the reviewed correspondence and is included
in its immutable digest. It is not transmitted as a new receiver API field.

The file must be regular, not a symlink, at most 16 KiB, and private (for example
`root:tgn-meetings` with mode `0440`, mounted read-only). Group write, execute and
other-user permissions are rejected. Require exactly the documented fields;
duplicate keys, invalid addresses, duplicate source/target addresses and reused
user IDs are rejected. Source, database and base URL must match the configured
destination. Up to 128 owners are supported. Addresses are normalized to lower
case and surrounding whitespace is removed.

Add `compose.odoo-owner-map.yaml` alongside the base Compose file and
`compose.odoo-sync.yaml` after installing `secrets/odoo_owner_map` privately.
Never commit the real mapping, paste its contents in logs or pass addresses as
command-line arguments. When sync is disabled, the file is not opened.

Migration v8 only adds a digest column to the destination binding. The first
activation seals the canonical map (or direct-address mode) in SQLite. Changing,
adding or removing a bound mapping requires a reviewed migration; a restart or
retry cannot silently alter the owner on an already attempted revision. Existing
v7 bindings can continue in direct-address mode but cannot acquire a mapping
automatically. Formatting, address case and row order do not change the digest.
An owner absent from a configured map fails locally with `owner_mapping_missing`
and sends no data. Delete events do not consult the correspondence and continue
to retract records even when the previous owner is no longer mapped.

Migration v7 does not enqueue existing history. New events generated while
disabled accumulate until activation. Review the intended scope before enabling.
For explicitly selected historical records use `odoo-sync-enqueue ID`;
regenerating historical analyses is a separate paid operation and is not done
by backfill.

Commands on the deployed binary:

- `odoo-sync-status`: read-only queue counts; never migrates or creates a DB.
- `odoo-sync-enqueue ID`: enqueue one existing, ready, uncancelled record;
  use only after checking authorization and ownership.
- `odoo-sync-retry ID`: retry one failed event, including a failed retraction,
  after resolving its cause.

Deploy the receiver before enabling the sender. Test a synthetic snapshot,
repeat delivery, a newer revision and deletion before enabling real data.
Use the existing migration/backup/rollback contract. A rollback must preserve
outbox state and cannot silently resurrect content removed on the receiver.

WebMCP: no public tool is created, modified or removed. The commercial view
remains authenticated and does not expose meeting data as a public tool.
