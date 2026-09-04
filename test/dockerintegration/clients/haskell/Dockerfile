# A real OPC XML-DA client, not a generic SOAP stack: mlabs-haskell's
# implementation hand-writes both the request construction and the
# response parser straight from the specification, and its parser is
# strict — an element the specification does not allow at that position
# is a hard decode error rather than something quietly skipped. That
# strictness is the whole point of testing against it.
#
# The dependency layer is expensive — a full GHC snapshot build, measured
# at ~9 minutes cold — but it depends only on stack.yaml and driver.cabal,
# so it is built once and reused for every driver change.
FROM haskell:9.0.2 AS build

WORKDIR /src
COPY test/dockerintegration/clients/haskell/stack.yaml \
     test/dockerintegration/clients/haskell/driver.cabal ./
RUN mkdir -p app && printf 'main :: IO ()\nmain = return ()\n' > app/Main.hs
RUN stack build --system-ghc --only-dependencies

COPY test/dockerintegration/clients/haskell/app/Main.hs app/Main.hs
RUN stack build --system-ghc --copy-bins --local-bin-path /out

# A second stage purely for size. The build stage carries a GHC
# installation and a compiled Stackage snapshot: 7.2 GB, against a 19 MB
# statically-linked-except-for-libc driver. That matters because CI
# runners are ephemeral and disk-constrained — the image is rebuilt on
# every run there, and 7.2 GB of it plus the layer cache is enough to
# fill an ubuntu-latest runner.
#
# bullseye rather than buster (which is what haskell:9.0.2 is built on):
# buster is end-of-life and its apt repositories have moved to
# archive.debian.org, so an apt-get there fails outright. glibc is
# forward-compatible, so a binary linked against buster's 2.28 runs on
# bullseye's 2.31. libgmp is GHC's Integer implementation and libnuma
# comes from its RTS; every other library the driver needs is in the base
# image (verified with ldd).
FROM debian:bullseye-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends libgmp10 libnuma1 \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/driver /usr/local/bin/driver

CMD ["driver"]
