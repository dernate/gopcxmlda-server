# The foreign-client image: a SOAP stack with no connection to this
# repository, driving the server from the specification's own WSDL.
#
# Build context must be the repository root, so testdata/schema (the WSDL
# and the XSD it imports) is visible:
#
#   docker build -f test/dockerintegration/foreignclient/Dockerfile \
#       -t gopcxmlda-foreignclient .
#
# foreignclient_test.go does this automatically.
FROM python:3.13-slim

# zeep is an independent, widely-used Python SOAP client. Pinned, because
# the point of this image is a stable second opinion, not the newest one.
# It builds its proxy from the WSDL alone and validates every response
# against the schema — the check neither integration suite can make with
# a client written by the same author as the server.
RUN pip install --no-cache-dir "zeep==4.3.1"

WORKDIR /app
COPY testdata/schema/opcxmlda.wsdl testdata/schema/opcxmlda.xsd ./
COPY test/dockerintegration/foreignclient/client.py ./
ENTRYPOINT ["python", "-u", "client.py"]
