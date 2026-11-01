-- Argo CD health check for CoderWorkspacePool (coder.com/v1alpha1).
-- Install it as the argocd-cm key
-- resource.customizations.health.coder.com_CoderWorkspacePool.
-- docs/how-to/gitops.md copies this file. A test keeps the copy equal.
local hs = { status = "Progressing", message = "Waiting for the controller" }
if obj.metadata.deletionTimestamp ~= nil then
  -- Argo CD shows deletionMessage, not message, for a resource being deleted.
  hs.deletionMessage = "Deleting the workspace pool"
  hs.message = hs.deletionMessage
  return hs
end
local st = obj.status
if st == nil or st.observedGeneration == nil or st.observedGeneration ~= obj.metadata.generation then
  return hs
end
-- A condition from an older generation does not describe the current spec.
local conditions = {}
for _, c in ipairs(st.conditions or {}) do
  if c.observedGeneration == obj.metadata.generation then
    table.insert(conditions, c)
  end
end
for _, c in ipairs(conditions) do
  if c.type == "Ready" and c.reason ~= nil and c.message ~= nil then
    hs.message = c.reason .. ": " .. c.message
  end
end
for _, c in ipairs(conditions) do
  if c.type == "Ready" and c.status == "True" then
    hs.status = "Healthy"
  elseif c.type == "Stalled" and c.status == "True" then
    hs.status = "Degraded"
  end
end
return hs
