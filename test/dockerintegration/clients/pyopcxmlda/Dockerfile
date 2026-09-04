# A real OPC XML-DA client: NothinRandom/pyopcxmlda builds every request
# by hand from the specification and parses every response with
# ElementTree, depending only on requests. It knows OPC XML-DA
# semantics -- item lists, subscriptions, qualities -- rather than SOAP
# in general, which is exactly what distinguishes it from the zeep
# harness next door.
#
# Pinned: a green run has to stay reproducible, and a client that
# silently changed its wire format would defeat the purpose.
FROM python:3.13-slim

RUN pip install --no-cache-dir "pyopcxmlda==0.1.3"

COPY test/dockerintegration/clients/pyopcxmlda/relax-optional-attributes.py /relax-optional-attributes.py
RUN python /relax-optional-attributes.py

COPY test/dockerintegration/clients/pyopcxmlda/client.py /client.py
CMD ["python", "-u", "/client.py"]
