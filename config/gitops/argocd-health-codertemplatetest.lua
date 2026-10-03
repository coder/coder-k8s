-- Argo CD health check for CoderTemplateTest (coder.com/v1alpha1).
-- Install it as the argocd-cm key
-- resource.customizations.health.coder.com_CoderTemplateTest.
-- docs/how-to/gitops.md copies this file. A test keeps the copy equal.
local hs = { status = "Progressing", message = "Waiting for the controller" }
if obj.metadata.deletionTimestamp ~= nil then
  -- Argo CD shows deletionMessage, not message, for a resource being deleted.
  hs.deletionMessage = "Deleting the test workspace"
  hs.message = hs.deletionMessage
  return hs
end
local st = obj.status
if st == nil or st.observedGeneration == nil or st.observedGeneration ~= obj.metadata.generation then
  return hs
end
if st.reason ~= nil and st.message ~= nil then
  hs.message = st.reason .. ": " .. st.message
end
if st.phase == "Succeeded" then
  for _, c in ipairs(st.conditions or {}) do
    if c.type == "Ready" and c.status == "True" then
      hs.status = "Healthy"
    end
  end
elseif st.phase == "Failed" then
  hs.status = "Degraded"
end
return hs
