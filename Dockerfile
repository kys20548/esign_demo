FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /esign .

FROM gcr.io/distroless/static-debian12
COPY --from=build /esign /esign
EXPOSE 8080
ENTRYPOINT ["/esign"]
