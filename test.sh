#!/bin/bash

set -e

API=http://localhost:8080/api/get-balance
KEY_HEADER="X-API-Key: dev-key-1"
CONTENT_HEADER="Content-Type: application/json"
VALID_WALLET="9ZPsRWGkukYeWg2Z7eZ8NaTBZ1DSuBUVzLcGQWZgE4Y4"

print_test() {
  echo -e "\n🔹 $1"
  echo "➡️  Request:"
  echo "curl -X POST $API -H \"$KEY_HEADER\" -H \"$CONTENT_HEADER\" -d '$2'"
  echo "⬅️  Response:"
  curl -s -X POST "$API" -H "$KEY_HEADER" -H "$CONTENT_HEADER" -d "$2"
  echo ""
}

echo "🚀 Starting test suite..."

# Test 1: Single wallet
print_test "Test 1: Single wallet" '{"wallets": ["'"$VALID_WALLET"'"]}'

# Test 2: Multiple wallets
print_test "Test 2: Multiple wallets" '{"wallets": ["'"$VALID_WALLET"'", "'"$VALID_WALLET"'", "'"$VALID_WALLET"'"]}'

# Test 3: 5 concurrent requests
echo -e "\n🔹 Test 3: 5 concurrent requests (same wallet)"
echo "➡️  Request (5 concurrent POSTs)..."
seq 1 5 | xargs -n1 -P5 -I{} sh -c \
  "curl -s -X POST $API -H '$KEY_HEADER' -H '$CONTENT_HEADER' -d '{\"wallets\": [\"$VALID_WALLET\"]}' && echo"
echo ""

# Test 4: Invalid API key
echo -e "\n🔹 Test 4: Invalid API key (should return 401)"
echo "➡️  Request:"
echo "curl -X POST $API -H \"X-API-Key: bad-key\" -H \"$CONTENT_HEADER\" -d '{\"wallets\": [\"$VALID_WALLET\"]}'"
echo "⬅️  Response:"
curl -s -w "\nHTTP %{http_code}\n" -X POST "$API" \
  -H "X-API-Key: bad-key" -H "$CONTENT_HEADER" \
  -d "{\"wallets\": [\"$VALID_WALLET\"]}"

# Test 5: Rate limit check
echo -e "\n🔹 Test 5: Rate limit check (11 requests, expect 429)"
for i in {1..11}; do
  code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$API" \
    -H "$KEY_HEADER" -H "$CONTENT_HEADER" \
    -d "{\"wallets\": [\"$VALID_WALLET\"]}")
  echo -n "$code "
done
echo ""

# Test 6: Caching
echo -e "\n🔹 Test 6: Caching timing (miss vs hit)"
echo "Warming cache..."
curl -s -X POST "$API" -H "$KEY_HEADER" -H "$CONTENT_HEADER" \
  -d "{\"wallets\": [\"$VALID_WALLET\"]}" > /dev/null
sleep 2
echo "➡️  First call (expect slower):"
time curl -s -X POST "$API" -H "$KEY_HEADER" -H "$CONTENT_HEADER" \
  -d "{\"wallets\": [\"$VALID_WALLET\"]}" > /dev/null
echo "➡️  Second call (expect fast from cache):"
time curl -s -X POST "$API" -H "$KEY_HEADER" -H "$CONTENT_HEADER" \
  -d "{\"wallets\": [\"$VALID_WALLET\"]}" > /dev/null

echo -e "\n✅ Test suite complete."