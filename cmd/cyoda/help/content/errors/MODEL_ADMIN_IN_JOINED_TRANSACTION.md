---
topic: errors.MODEL_ADMIN_IN_JOINED_TRANSACTION
title: "MODEL_ADMIN_IN_JOINED_TRANSACTION — model and workflow administration cannot run inside a transaction"
stability: stable
see_also:
  - errors
  - models
  - workflows
  - cluster
  - errors.COMMIT_IN_JOINED_TRANSACTION
---

# errors.MODEL_ADMIN_IN_JOINED_TRANSACTION

## NAME

MODEL_ADMIN_IN_JOINED_TRANSACTION — a request that carries a transaction token asked to change a model or its workflows, and was refused.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

A request carrying a transaction token — the `X-Tx-Token` header, or the `tx-token` gRPC metadata a compute member echoes on a callback — runs in the transaction of the operation that called the member out. Model and workflow administration never runs there: a model's schema, its lock state, its change level, its unique keys, its existence and its workflows are configuration, changed by an operation of their own and never as a step of a running transition.

The refused operations are the ones that change a model or its workflows:

- `POST /model/import/{dataFormat}/{converter}/{entityName}/{modelVersion}`
- `DELETE /model/{entityName}/{modelVersion}`
- `POST /model/{entityName}/{modelVersion}/changeLevel/{changeLevel}`
- `PUT /model/{entityName}/{modelVersion}/lock`
- `PUT /model/{entityName}/{modelVersion}/unlock`
- `PUT /model/{entityName}/{modelVersion}/unique-keys`
- `POST /model/{entityName}/{modelVersion}/workflow/import`
- over gRPC, `entityModelManage` with `EntityModelImportRequest`, `EntityModelTransitionRequest`, `EntityModelDeleteRequest` or `EntityModelSetUniqueKeysRequest`

The refusal comes first: before the token is verified, before any transaction lock is taken, before the request body is read and before anything is written. A forged or expired token on one of these operations is answered with this code, not with `401` or `410`. The transaction the token names is untouched.

The read-only model and workflow operations — list, export, validate, workflow export, and over gRPC `EntityModelExportRequest` and `EntityModelGetAllRequest` — are not administration and are not refused.

Not retryable: the same request meets the same refusal. Make the request without the token, as an independent request of its own, and let the processor handle its outcome. Over gRPC the code is the message prefix of the `CLIENT_ERROR` envelope.

## SEE ALSO

- errors
- models
- workflows
- cluster
- errors.COMMIT_IN_JOINED_TRANSACTION
