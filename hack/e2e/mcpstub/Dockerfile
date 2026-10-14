# The Kind E2E stub image (#155). CI builds the static binary first and runs
# docker build with the binary's directory as the context.
FROM gcr.io/distroless/static:nonroot

ENTRYPOINT ["/mcpstub"]
COPY mcpstub /mcpstub
USER nonroot:nonroot
