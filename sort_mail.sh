#!/bin/bash

# Script: sort_by_first_field.sh
# Usage: ./sort_by_first_field.sh [filename]
# Sorts lines using '@' as field delimiter, based on the first field only.
# If no filename is provided, reads from standard input.

# Set the filename from the first argument, or default to "filename.txt" if not provided
filename="${1:-filename.txt}"

# Check if input is from stdin or a file that exists
if [ "$#" -eq 0 ]; then
    # No argument: read from stdin
    sort -t '@' -k1,1
elif [ -f "$filename" ]; then
    # File exists: sort the file
    sort -t '@' -k1,1 "$filename"
else
    echo "Error: File '$filename' not found." >&2
    exit 1
fi
