# AI providers with CoderAIProvider

A user declares one AI provider of Coder (Coder's `/api/v2/ai/providers`) as a `CoderAIProvider` object, with the API key in a Secret that opted in to the credential contract. The operator creates the provider in Coder under the object's name, keeps its settings and key equal to the spec, disables it in Coder when the key Secret loses its consent (the credential kill switch), and deletes it in Coder when the object is deleted. The object reports the conditions `Ready`, `Reconciling` and `Stalled`, and `status.providerID`.

## Sub-features

- Create and update from `spec.type`, `spec.baseURL`, `spec.displayName`, `spec.enabled`, `spec.apiKeySecretRef`.
- Key rotation: a changed Secret value sends one update with the new key.
- Credential contract: label `coder.com/credential=true` and annotation `coder.com/credential-destinations=<origin of spec.baseURL>`.
- Kill switches: credential errors and dashboard changes stall the object (`CredentialNotOptedIn`, `DisabledExternally`, `KeyChangedExternally`, `DeletedExternally`).
- Delete with the finalizer `coder.com/ai-provider-cleanup` and a cleanup record ConfigMap `coder-system/ck8s-cleanup-<object UID>`.

## How to get to it (user POV)

Docs: `docs/how-to/manage-ai-providers.md`. Sample: `config/samples/coder_v1alpha1_coderaiprovider.yaml`. The user creates the key Secret, labels and annotates it, then applies the provider object that points at `controlPlaneRef.name: coder`.

## Driving it with kubectl

Preconditions: the baseline only. No license is needed. The key is a throwaway value: Coder stores it and does not call the upstream on create.

- **Create the key Secret.** `"$V" capture aip-secret -- bash -c '"$V" kubectl -n coder create secret generic verify-openai-key --from-literal=api-key="sk-verify-$(od -An -N8 -tx1 /dev/urandom | tr -d " \n")" && "$V" kubectl -n coder label secret verify-openai-key coder.com/credential=true && "$V" kubectl -n coder annotate secret verify-openai-key coder.com/credential-destinations=https://api.openai.com'` prints `created`, `labeled` and `annotated`.
- **Create the provider.** `"$V" capture aip-apply -- bash -c 'printf "%s\n" "apiVersion: coder.com/v1alpha1" "kind: CoderAIProvider" "metadata: {name: verify-openai, namespace: coder}" "spec:" "  controlPlaneRef: {name: coder}" "  type: openai" "  baseURL: https://api.openai.com/v1" "  displayName: Verify OpenAI" "  apiKeySecretRef: {name: verify-openai-key, key: api-key}" | "$V" kubectl apply -f -'` prints `coderaiprovider.coder.com/verify-openai created`.
- **Wait for Ready.** `"$V" capture aip-ready -- "$V" kubectl -n coder wait --for=condition=Ready coderaiprovider/verify-openai --timeout=2m` prints `condition met` (usually under 1 second). `"$V" kubectl -n coder get coderaiprovider verify-openai -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'` shows `Ready=True Applied`.
- **Prove the provider in Coder.** `"$V" capture aip-coder -- "$V" coder-api GET /api/v2/ai/providers/verify-openai` returns the provider with `display_name` `Verify OpenAI`, `enabled` true, the base URL, and an `id` equal to `status.providerID`. The key comes back masked.
- **Pull the credential consent.** `"$V" capture aip-killswitch -- "$V" kubectl -n coder label secret verify-openai-key coder.com/credential-`. Within seconds the object has `Stalled=True CredentialNotOptedIn` (`"$V" kubectl -n coder wait --for=condition=Stalled coderaiprovider/verify-openai --timeout=1m`), and `"$V" coder-api GET /api/v2/ai/providers/verify-openai` shows `enabled` false.
- **Restore it.** Label the Secret again (`"$V" kubectl -n coder label secret verify-openai-key coder.com/credential=true`). The object now shows `Stalled=True DisabledExternally`, because Coder holds the disabled provider. Then edit the spec (`"$V" kubectl -n coder patch coderaiprovider verify-openai --type=merge -p '{"spec":{"displayName":"Verify OpenAI 2"}}'`). `Ready=True Applied` returns, and Coder shows `enabled` true and the new display name.
- **Delete it.** `"$V" capture aip-delete -- "$V" kubectl -n coder delete coderaiprovider verify-openai --timeout=2m` returns once the finalizer is gone. Then `"$V" capture aip-gone -- "$V" coder-api GET /api/v2/ai/providers/verify-openai` answers `HTTP 404`, and `"$V" kubectl -n coder-system get configmaps -l coder.com/cleanup-record` lists no record for it.

## Gotchas

- Repairing the Secret does not enable the provider again. Only a spec change does. Between the repair and the spec change the reason is `DisabledExternally`, although the operator itself disabled the provider. This is the documented kill-switch behavior, not a bug.
- The operator never adopts a provider that it did not create: a name that Coder already has stalls with `NameConflict`.
- A control plane `extraEnv` such as `CODER_AI_GATEWAY_OPENAI_KEY` reserves the name `openai` (`NameReservedByEnv`). The baseline sets none, but use a `verify-` name anyway.
- Delete providers before the control plane and before namespace `coder`. In a namespace deletion the provider takes the `ControlPlaneGone` path, stays in Coder, and leaves a cleanup record in `coder-system`.
- A create logs 2 or 3 harmless `update status ... object has been modified` Reconciler errors in the operator log. They are retried.
- Dashboard changes show up only at the next read: the resync is about 10 minutes, or at once when the object, its Secret or its control plane changes.
