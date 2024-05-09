# Use an official Golang image as a base
FROM golang:alpine

# Set the working directory to /app
WORKDIR /app

# Copy the current directory contents into the container at /app
COPY main.go /app
COPY go.mod /app
# Install any needed packages specified in go.mod
RUN go mod download

# Build the Go program
RUN go build -o main main.go

# Make port 8080 available to the world outside this container
EXPOSE 8080

# Define environment variable
ENV PORT 8080

# Run main when the container launches
CMD ["./main"]