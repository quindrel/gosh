# `srs/get_email_templates.json` — `template` parameter is required but no value is accepted

**Filed:** 2026-05-04 (during examples/srs-tier3-writes validation)
**Status:** open

## Summary

`GET /1.5/srs/get_email_templates.json` validates `template` as
required ("The template name is missing." when omitted) but no
value derivable from the corresponding `list_email_templates`
response is accepted. Every probed value returns "The specified
template doesn't exist, or you don't have access to it."

## Reproduction

`list_email_templates.json` returns entries shaped like:

```json
{
  "template_id": "11239",
  "client_id": "979387",
  "type": "AutoRenewReminder",
  "subject": "Domain Auto-Renew Reminder -  {DOMAINNAME}",
  "template": "Hi {CUSTOMERNAME}, ...",
  "name": "Auto-Renew Reminder - 7 Days",
  "customized": true
}
```

Probed `template=` values against the same client that returned
that list:

| value                                | response                                  |
| ------------------------------------ | ----------------------------------------- |
| `Auto-Renew Reminder - 7 Days` (name)| `The specified template doesn't exist…`   |
| `AutoRenewReminder` (type)           | `The specified template doesn't exist…`   |
| `11239` (template_id)                | `The specified template doesn't exist…`   |

Probed alternate parameter names:

| param                          | response                       |
| ------------------------------ | ------------------------------ |
| `template_id=11239`            | `The template name is missing.`|
| `name=AutoRenewReminder`       | `The template name is missing.`|
| `type=AutoRenewReminder`       | `The template name is missing.`|

So `template` is the right param name (it's the only key whose
omission triggers "missing"), but no value derived from the
sibling list endpoint is accepted by it.

## Why this matters

`update_email_template.json` accepts `template=ExampleTemplate` per
the docs but there's no documented way to *read* the current value
of a template before updating it — meaning every customer-facing
template edit is a blind write. List returns the bodies, so the
current text is observable, but a "read one template" round-trip
isn't usable.

## Open questions for the API team

1. **What value does `template` accept?** Is it a slug not exposed
   anywhere on the list response? A registry-internal name?
2. **Should the docs page populate its Form Parameters table?**
   Currently empty for this endpoint.
3. **Could `update_email_template`'s docs include the same
   `template=` name discovery so callers can predict round-trips?**
