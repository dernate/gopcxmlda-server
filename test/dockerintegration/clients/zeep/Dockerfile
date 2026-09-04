# A generic SOAP stack, not an OPC XML-DA client: zeep knows SOAP and
# XML Schema and nothing about this protocol, and builds its proxy from
# the specification's own WSDL. What it therefore checks is schema
# conformance -- it raises rather than ignoring content the schema does
# not allow, which is exactly where Go's encoding/xml is lenient.
#
# It is not an independent second opinion on OPC semantics: the WSDL it
# is driven from was transcribed from the specification's appendix in
# this repository, so a mistake in that transcription would be invisible
# here. The clients that do give that second opinion are the two real
# OPC XML-DA implementations next door (clients/pyopcxmlda and
# clients/haskell).
#
# Build context must be the repository root, so testdata/schema (the WSDL
# and the XSD it imports) is visible:
#
#   docker build -f test/dockerintegration/clients/zeep/Dockerfile \
#       -t gopcxmlda-zeepclient .
#
# foreignclients_test.go does this automatically.
FROM python:3.13-slim

# zeep is an independent, widely-used Python SOAP client. Pinned, because
# the point of this image is a stable second opinion, not the newest one.
# It builds its proxy from the WSDL alone and validates every response
# against the schema — the check neither integration suite can make with
# a client written by the same author as the server.
RUN pip install --no-cache-dir "zeep==4.3.1"

WORKDIR /app
COPY testdata/schema/opcxmlda.wsdl testdata/schema/opcxmlda.xsd ./
COPY test/dockerintegration/clients/zeep/client.py ./
ENTRYPOINT ["python", "-u", "client.py"]
