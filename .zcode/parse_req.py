import json, urllib.parse
for line in open("/tmp/oai-probe/requests.jsonl", errors="ignore"):
    r = json.loads(line)
    u = str(r.get("url") or "")
    kind = r.get("kind")
    if kind == "request" and "signin/openai" in u:
        q = urllib.parse.parse_qs(urllib.parse.urlparse(u).query)
        print("signin keys", sorted(q))
        print("ext-oai-did", q.get("ext-oai-did"))
        print("login_hint", q.get("login_hint"))
        print("QUERY", urllib.parse.urlparse(u).query)
    if kind == "request" and "/api/accounts/authorize" in u:
        q = urllib.parse.parse_qs(urllib.parse.urlparse(u).query)
        print("authorize login_hint", q.get("login_hint"))
        print("authorize device_id", q.get("device_id"))
        print("authorize ext-oai-did", q.get("ext-oai-did"))
        print("authorize keys", sorted(q))
        print("AUTHQ", urllib.parse.urlparse(u).query)
