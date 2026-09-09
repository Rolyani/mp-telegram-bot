FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /mp-bot ./cmd/mp-bot

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /mp-bot /mp-bot
ENTRYPOINT ["/mp-bot"]

