# ferry's rendezvous: Concord's rendezvous node, built unmodified from a
# pinned release. ferry speaks its protocols (DHT provider records, circuit
# relay v2, mailbox) and needs nothing else from it.
#
# cmd/rendezvous is a main package built on Concord's internal packages, so
# it cannot be imported from this module; it is built from source instead.
FROM golang:1.26 AS build
ARG CONCORD_REF=v0.59.1
ARG CONCORD_SHA=770aee6ffca4ff7fedc032ecf7724dd63fe096ef
RUN git clone --depth 1 --branch "$CONCORD_REF" https://github.com/ZahakJ/concord /src \
 && test "$(git -C /src rev-parse HEAD)" = "$CONCORD_SHA"
WORKDIR /src
RUN CGO_ENABLED=0 go build -trimpath -o /rendezvous ./cmd/rendezvous

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /rendezvous /rendezvous
ENV PORT=4001
EXPOSE 4001/tcp 4001/udp
ENTRYPOINT ["/rendezvous"]
