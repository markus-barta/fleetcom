# PPM Mode — Read-Only Project Overview

You are in **PPM mode**. This is a read-only session focused on project planning and oversight using PPM.

## Constraints

- **DO NOT** modify any local files, configs, or code unless the user explicitly says "this is an exception"
- **DO NOT** build, deploy, or provision anything
- **DO** read local files for context (docs, configs, etc.)
- **DO** interact with PPM freely: query, create tickets, update statuses, add comments
- **DO** manage time entries: start/stop timers, log flat hours

## PPM Context

- **Base URL**: `https://pm.barta.cm`
- **Project**: DSC Infrastructure (project ID: 2, key: DSC26)
- **FleetCom Epic**: DSC26-52 (parent for all FleetCom tickets)
- **Auth**: `Authorization: Bearer $PPMAPIKEY`

## Key API Endpoints

| Action | Method | Endpoint |
|--------|--------|----------|
| List project issues | GET | `/api/projects/2/issues` |
| Issue tree (hierarchy) | GET | `/api/projects/2/issues/tree` |
| Single issue | GET | `/api/issues/{id}` |
| Create issue | POST | `/api/projects/2/issues` |
| Update issue | PUT | `/api/issues/{id}` |
| Issue children | GET | `/api/issues/{id}/children` |
| Issue comments | GET | `/api/issues/{id}/comments` |
| Add comment | POST | `/api/issues/{id}/comments` body: `{body}` |
| Search | GET | `/api/search?q=...` |
| Time entries | GET | `/api/issues/{id}/time-entries` |
| Create time entry | POST | `/api/issues/{id}/time-entries` |
| Update time entry | PUT | `/api/time-entries/{id}` |

### Filtering

`?status=new,in-progress` `?priority=high` `?type=epic,ticket` `?limit=50&offset=0`

### Issue Fields for Create/Update

```json
{
  "title": "...",
  "type": "ticket|epic|task",
  "status": "new|backlog|in-progress|qa|done|accepted|invoiced|cancelled",
  "priority": "low|medium|high",
  "description": "...",
  "acceptance_criteria": "...",
  "parent_id": 172
}
```

Note: `parent_id: 172` = DSC26-52 (FleetCom epic). Use this for all FleetCom tickets.

### Time Tracking

- mba user_id: 2
- Start: `POST /api/issues/{id}/time-entries` with `{"user_id": 2}`
- Stop: `PUT /api/time-entries/{id}` with `{"stopped_at": "<ISO8601>"}`
- Flat: `POST /api/issues/{id}/time-entries` with `{"user_id": 2, "override": 1.5, "comment": "..."}`

## Default Behavior

When invoked without arguments, show FleetCom project dashboard:

1. Fetch FleetCom children: `GET /api/issues/172/children`
2. Present summary: tickets by status, highlight actionable items
3. Show running time entries
4. Cross-reference with DSC26 project for related work
