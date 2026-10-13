# Configure credential Secrets for proxies and provisioners

The operator sends Secret values to Coder and puts them into `CoderWorkspaceProxy` and `CoderProvisioner` pods. It does this only for Secrets that you mark as credentials for a destination. This page shows how to mark a Secret, which checks the operator runs, and how to upgrade from a version without these checks.

!!! warning "Who may write these resources"
    Grant `create`, `update` and `patch` on `coderworkspaceproxies`, `coderprovisioners` and `codercontrolplanes` only to principals that may already read Secrets and create Pods in that namespace. A custom `spec.image` runs with everything the pod receives: the proxy token or the provisioner key, every `extraEnv` value, and for a `CoderProvisioner` the ServiceAccount token that manages workspace pods. The credential checks do not restrict the image.

## Mark a Secret as a credential

A Secret is usable only when it has both of these:

- The label `coder.com/credential: "true"`.
- The annotation `coder.com/credential-destinations` with the origins that may receive its values. An origin is `scheme://host`, plus `:port` for a port that is not the default. Separate more than one origin with commas.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: coderworkspaceproxy-token
  namespace: coder
  labels:
    coder.com/credential: "true"
  annotations:
    coder.com/credential-destinations: "https://coder.example.com"
type: Opaque
stringData:
  token: "<workspace proxy token>"
```

Or mark an existing Secret:

```bash
kubectl -n coder label secret coderworkspaceproxy-token coder.com/credential=true
kubectl -n coder annotate secret coderworkspaceproxy-token coder.com/credential-destinations=https://coder.example.com
```

The destination of each Secret input:

| Kind | Secret input | Destination origin |
| --- | --- | --- |
| `CoderWorkspaceProxy` | `spec.proxySessionTokenSecretRef` | `spec.primaryAccessURL` |
| `CoderWorkspaceProxy` | `spec.bootstrap.credentialsSecretRef` | `spec.bootstrap.coderURL`, and also the primary access URL (`spec.primaryAccessURL`, or `spec.bootstrap.coderURL` when it is empty), because the generated proxy token comes from this credential |
| `CoderWorkspaceProxy` | every `spec.extraEnv[].valueFrom.secretKeyRef` | the primary access URL |
| `CoderProvisioner` | every `spec.extraEnv[].valueFrom.secretKeyRef` | `status.url` of the referenced `CoderControlPlane` |

Get the control plane URL for a provisioner:

```bash
kubectl -n coder get codercontrolplane <name> -o jsonpath='{.status.url}{"\n"}'
```

## Checks the operator runs

The operator also refuses these Secrets, even with the label and the annotation:

- Secrets that the operator generates: Secrets owned by a `coder.com` resource, Secrets with the label `app.kubernetes.io/managed-by=coder-k8s`, and the operator token Secret of any `CoderControlPlane` in the namespace (for example `<control plane>-operator-token`).
- The license Secret (`spec.licenseSecretRef`) and the database Secret (`spec.database.connectionSecretRef`) of any `CoderControlPlane` in the namespace. The message names the control plane, for example `Secret "coder-db" is the database Secret of CoderControlPlane "coder" and cannot be used here.`
- Secrets with a type other than `Opaque`.
- Values that are empty, larger than 8 KiB, or that start or end with whitespace (a trailing newline counts).

It also checks these settings:

- `spec.extraEnv` and `spec.extraArgs` must not override the URL or the token that the operator sets:

    | Kind | Reserved `extraEnv` names | Reserved `extraArgs` flags |
    | --- | --- | --- |
    | `CoderWorkspaceProxy` | `CODER_PRIMARY_ACCESS_URL`, `CODER_PROXY_SESSION_TOKEN` | `--primary-access-url`, `--proxy-session-token` |
    | `CoderProvisioner` | `CODER_URL`, `CODER_PROVISIONER_DAEMON_KEY` | `--url`, `--key` |

    Env names match without case, because the Coder CLI reads them without case. A flag matches with one or two dashes, and with or without `=value`. The operator checks each arg after the `$(VAR)` expansion that Kubernetes applies to container args. It rejects an arg where a variable whose value it cannot see (an operator-set variable, or a `valueFrom` variable) can change the flag name: a reference at the start of the arg, or before the `=` of a flag. A reference in a flag value, such as `--git-token=$(GIT_TOKEN)`, passes. Escape such a reference as `$$(VAR)` when you want the literal text.

- With `spec.bootstrap`, the bootstrap credentials Secret must exist on every reconcile, also after the first registration. The operator checks that its destinations include the primary access URL, because the generated proxy token comes from it.
- With `spec.bootstrap`, the generated token Secret (`spec.bootstrap.generatedTokenSecretName`, default `<proxy>-proxy-token`) must not exist yet, or must have this proxy as its controller owner. The operator does not read, mount, change or adopt any other Secret with that name.
- The provisioner key Secret of a `CoderProvisioner` (`spec.key.secretName`, default `<provisioner>-provisioner-key`) must not exist yet, or must have this provisioner as its controller owner. When it does not exist, the operator creates it. The operator does not read, mount, change or adopt any other Secret with that name. Earlier versions adopted an unowned Secret with that name on the first reconcile. Such an adopted Secret already has this provisioner as its controller owner, so the operator keeps using it, and Kubernetes deletes it with the provisioner.

With `spec.bootstrap`, `spec.bootstrap.coderURL` must be the final origin of the Coder API: the scheme, host and port that answer without a redirect. The operator sends the bootstrap credentials only to that origin. When Coder answers with a redirect to a different origin, for example from `http://` to `https://` or to a canonical host, the operator does not follow it, and the proxy registration fails. `CredentialsReady` stays `True`, because the credential checks passed. The operator log shows `Reconciler error` with `refusing redirect to a different origin than the coder URL`, and the operator retries with a backoff. To fix it, set `spec.bootstrap.coderURL` to the final URL. A redirect to the same origin still works.

Proxy pods run with `automountServiceAccountToken: false`. The proxy talks only to coderd and does not need a Kubernetes API token. Provisioner pods keep their token.

## Read the result

Each `CoderWorkspaceProxy` and `CoderProvisioner` has the condition `CredentialsReady`. When a check fails, the condition is `False`, and the operator sends nothing to Coder and does not change the Deployment. A Deployment that already runs keeps its last applied configuration. The operator applies spec changes again after you fix the cause.

```bash
kubectl get coderworkspaceproxies,coderprovisioners -A -o jsonpath='{range .items[*]}{.kind} {.metadata.namespace}/{.metadata.name}: {.status.conditions[?(@.type=="CredentialsReady")].status} {.status.conditions[?(@.type=="CredentialsReady")].reason}{"\n"}{end}'
```

| Reason | Cause | Fix |
| --- | --- | --- |
| `CredentialSecretNotFound` | The referenced Secret does not exist. | Create the Secret. |
| `CredentialKeyMissing` | The Secret does not have the referenced key. | Add the key, or fix the reference. |
| `CredentialNotOptedIn` | The label or the annotation is missing. | Add both. |
| `CredentialDestinationNotAllowed` | The annotation does not list the destination origin. | Add the origin that the message names. |
| `CredentialRefused` | The operator generated the Secret, a `CoderControlPlane` in the namespace uses it as its license or database Secret, or the type is not `Opaque`. | Store the credential in its own `Opaque` Secret. For a proxy, create a dedicated Coder token instead of the operator token. |
| `CredentialMalformed` | The value or the annotation is not valid, or a URL or a required reference in the spec is missing or not valid. | Fix what the message names. |
| `TokenSecretNotOwned` | The generated token Secret exists and another owner, or no owner, controls it. | Remove `generatedTokenSecretName` to use the default name, or set a name that does not exist yet. |
| `KeySecretNotOwned` | The provisioner key Secret exists and another owner, or no owner, controls it. | Delete that Secret, or set `spec.key.secretName` to a name that does not exist yet. The operator then creates a new provisioner key in Coder. |
| `ReservedSettingOverride` | `extraEnv` or `extraArgs` sets a reserved name or flag. | Remove the entry. |

Messages name Secrets, keys, fields and origins. They never contain Secret values.

A Deployment that already runs keeps its last applied configuration after a failed check. This includes any reference it already has to a Secret that the operator now refuses. The references go away at the first rollout after you fix the cause.

## Upgrade from a version without these checks

Do these steps before you upgrade the operator. Resources that fail a check after the upgrade keep running with their last applied configuration, so an upgrade causes no outage. But the operator does not apply their spec changes until you fix them.

1. List the referenced Secrets. Lines that start with `consent` need the label and the annotation. Lines that start with `owned` name a generated proxy token Secret or a provisioner key Secret, which must not exist or must be owned by that resource:

    ```bash
    kubectl get coderworkspaceproxies,coderprovisioners -A -o json | jq -r '.items[] |
      .kind as $k | .metadata.namespace as $ns | .metadata.name as $cr |
      (([.spec.bootstrap.credentialsSecretRef.name?, .spec.proxySessionTokenSecretRef.name?]
        + [.spec.extraEnv[]?.valueFrom.secretKeyRef.name?]) | map(select(. != null)) | unique[] |
        "consent \($ns)/\(.) (\($k) \($cr))"),
      (if $k == "CoderWorkspaceProxy" and .spec.bootstrap then
         "owned \($ns)/\(.spec.bootstrap.generatedTokenSecretName // ($cr + "-proxy-token")) (\($k) \($cr) uid=\(.metadata.uid))"
       elif $k == "CoderProvisioner" then
         "owned \($ns)/\(.spec.key.secretName // ($cr + "-provisioner-key")) (\($k) \($cr) uid=\(.metadata.uid))"
       else empty end)'
    ```

2. Check the owner of each `owned` Secret. Lines that start with `ACTION` name a Secret that the operator refuses after the upgrade. The script reads only owner references, not Secret data:

    ```bash
    kubectl get coderworkspaceproxies,coderprovisioners -A -o json | jq -r '.items[] |
      (if .kind == "CoderProvisioner" then .spec.key.secretName // (.metadata.name + "-provisioner-key")
       elif .spec.bootstrap then .spec.bootstrap.generatedTokenSecretName // (.metadata.name + "-proxy-token")
       else empty end) as $s |
      "\(.metadata.namespace) \($s) \(.metadata.uid) \(.kind)/\(.metadata.name)"' |
    while read -r ns name uid cr; do
      out=$(kubectl -n "$ns" get secret "$name" --ignore-not-found \
        -o jsonpath='found {.metadata.ownerReferences[?(@.controller==true)].uid}') ||
        { echo "ERROR   $ns/$name ($cr): kubectl failed"; continue; }
      case $out in
        "") echo "ok      $ns/$name ($cr): does not exist yet" ;;
        "found $uid") echo "ok      $ns/$name ($cr): owned" ;;
        *) echo "ACTION  $ns/$name ($cr): not owned by $cr" ;;
      esac
    done
    ```

    For each `ACTION` line:

    - For a proxy, remove `generatedTokenSecretName`, or set it to a name that does not exist yet.
    - For a provisioner, delete the Secret, or set `spec.key.secretName` to a name that does not exist yet. In both cases the operator creates a new provisioner key in Coder, stores it in a new Secret that it owns, and rolls out the provisioner pods once. The old key stops working when the operator replaces it. The operator does not delete a Secret that it does not own.

    Also check each `owned` Secret for data keys that the operator does not manage. An owned proxy token Secret must hold only the `token` key, and an owned provisioner key Secret only its `spec.key.secretKey` (default `key`). Another key means that an earlier version adopted a Secret that you created. Kubernetes deletes that Secret, with all its keys, when you delete the resource. Move any other key to its own Secret.

3. List proxies and provisioners with reserved overrides, and remove those entries:

    ```bash
    kubectl get coderworkspaceproxies,coderprovisioners -A -o json | jq -r '.items[] |
      (if .kind == "CoderWorkspaceProxy" then ["CODER_PRIMARY_ACCESS_URL", "CODER_PROXY_SESSION_TOKEN"]
       else ["CODER_URL", "CODER_PROVISIONER_DAEMON_KEY"] end) as $env |
      (if .kind == "CoderWorkspaceProxy" then "^--?(primary-access-url|proxy-session-token)(=|$)"
       else "^--?(url|key)(=|$)" end) as $arg |
      select(([.spec.extraEnv[]?.name | ascii_upcase] | any(IN($env[]))) or ([.spec.extraArgs[]?] | any(test($arg)))) |
      "\(.kind) \(.metadata.namespace)/\(.metadata.name)"'
    ```

    The operator also rejects an `extraArgs` entry that starts with `$(VAR)`, or has `$(VAR)` before the `=` of a flag, when the variable is set from `valueFrom` or by the operator. Search for `$(` in `extraArgs` and check each match.

4. For each `consent` Secret, add the label and the destinations annotation. If a bootstrap credentials Secret no longer exists, create it again (any valid token for that Coder deployment), labeled and annotated. Use the destination table in [Mark a Secret as a credential](#mark-a-secret-as-a-credential):

    ```bash
    kubectl -n <ns> label secret <name> coder.com/credential=true
    kubectl -n <ns> annotate secret <name> coder.com/credential-destinations=https://coder.example.com
    ```

5. If a proxy uses the operator token as `spec.bootstrap.credentialsSecretRef`, create a dedicated Coder API token for it. Store the token in its own labeled and annotated Secret, and point `credentialsSecretRef` at that Secret. No label makes the operator token usable.

    The operator also refuses the license Secret and the database Secret of a `CoderControlPlane` in the same namespace. List the proxies and provisioners that reference one of them:

    ```bash
    kubectl get codercontrolplanes,coderworkspaceproxies,coderprovisioners -A -o json | jq -r '
      [.items[] | select(.kind == "CoderControlPlane") | .metadata.namespace as $ns |
        (.spec.licenseSecretRef.name?, .spec.database.connectionSecretRef.name?) | select(. != null) | "\($ns)/\(.)"] as $used |
      .items[] | select(.kind == "CoderWorkspaceProxy" or .kind == "CoderProvisioner") |
      .kind as $k | .metadata.namespace as $ns | .metadata.name as $cr |
      ([.spec.bootstrap.credentialsSecretRef.name?, .spec.proxySessionTokenSecretRef.name?]
        + [.spec.extraEnv[]?.valueFrom.secretKeyRef.name?]) | map(select(. != null) | "\($ns)/\(.)") | unique[] |
      select(IN($used[])) | "ACTION \(.) (\($k) \($cr))"'
    ```

    For each `ACTION` line, copy the value that the resource needs into its own labeled and annotated Secret, and point the resource at that Secret.

6. If a custom proxy image reads the Kubernetes API, change it. Proxy pods no longer get a service account token. Each proxy that passes the checks rolls out once after the upgrade because of this change.

7. Upgrade the operator.

8. Check every proxy and provisioner. Every line must end in `True`:

    ```bash
    kubectl get coderworkspaceproxies,coderprovisioners -A -o jsonpath='{range .items[*]}{.kind} {.metadata.namespace}/{.metadata.name}: {.status.conditions[?(@.type=="CredentialsReady")].status}{"\n"}{end}'
    ```
