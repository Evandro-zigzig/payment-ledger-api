FROM golang:1.26-alpine3.23 AS builder

RUN apk add --no-cache ca-certificates git

