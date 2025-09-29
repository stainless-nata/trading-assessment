#!/bin/bash

API_URL="http://localhost:8080"
API_KEY="test-api-key-1"

echo "🧪 Testing API..."

# Test 1: Health check
echo "1. Health check"
curl -s "$API_URL/health"
echo -e "\n"

# Test 2: Single wallet
echo "2. Single wallet balance"
curl -X POST "$API_URL/api/get-balance" \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $API_KEY" \
  -d '{"wallets": ["11111111111111111111111111111112"]}'
echo -e "\n"

# Test 3: Multiple wallets
echo "3. Multiple wallets"
curl -X POST "$API_URL/api/get-balance" \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $API_KEY" \
  -d '{"wallets": ["11111111111111111111111111111112", "So11111111111111111111111111111111111111112"]}'
echo -e "\n"

# Test 4: Rate limiting
echo "4. Rate limiting (12 requests)"
for i in {1..12}; do
    response=$(curl -s -w "HTTP_%{code}" -X POST "$API_URL/api/get-balance" \
      -H "Content-Type: application/json" \
      -H "X-API-Key: $API_KEY" \
      -d '{"wallets": ["11111111111111111111111111111112"]}')
    echo "Request $i: $response"
    if [[ $response == *"429"* ]]; then
        echo "Rate limiting working!"
        break
    fi
done

echo "Tests completed!"
