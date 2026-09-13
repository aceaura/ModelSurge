import 'dart:async';
import 'dart:convert';

import 'package:http/http.dart' as http;

class Upstream {
  final String name;
  final String provider;
  final String protocol;
  final String baseUrl;
  final List<String> models;
  final bool available;
  final String status;
  final String reason;
  final double? lastBalance;
  final String? lastBalanceAt;
  final int consecFail;
  final int requests;
  final int successes;
  final int lastLatencyMs;

  Upstream.fromJson(Map<String, dynamic> j)
      : name = j['name'] ?? '',
        provider = j['provider'] ?? '',
        protocol = j['protocol'] ?? '',
        baseUrl = j['base_url'] ?? '',
        models = (j['models'] as List? ?? []).cast<String>(),
        available = j['available'] ?? false,
        status = j['state']?['status'] ?? 'unknown',
        reason = j['state']?['reason'] ?? '',
        lastBalance = (j['state']?['last_balance'] as num?)?.toDouble(),
        lastBalanceAt = j['state']?['last_balance_at'],
        consecFail = j['state']?['consec_fail'] ?? 0,
        requests = j['state']?['requests'] ?? 0,
        successes = j['state']?['successes'] ?? 0,
        lastLatencyMs = j['state']?['last_latency_ms'] ?? 0;
}

class ModelEntry {
  final String name;
  final int upstreams;
  final int available;
  ModelEntry.fromJson(Map<String, dynamic> j)
      : name = j['name'] ?? '',
        upstreams = j['upstreams'] ?? 0,
        available = j['available'] ?? 0;
}

class GatewayEvent {
  final int seq;
  final String at;
  final String upstream;
  final String from;
  final String to;
  final String reason;
  GatewayEvent.fromJson(Map<String, dynamic> j)
      : seq = j['seq'] ?? 0,
        at = j['at'] ?? '',
        upstream = j['upstream'] ?? '',
        from = j['from'] ?? '',
        to = j['to'] ?? '',
        reason = j['reason'] ?? '';
}

class ApiClient {
  final String baseUrl; // e.g. http://127.0.0.1:18099
  final String token;
  final http.Client _http = http.Client();

  ApiClient(this.baseUrl, this.token);

  Map<String, String> get _headers => {'Authorization': 'Bearer $token'};

  Future<List<Upstream>> upstreams() async {
    final r = await _http.get(Uri.parse('$baseUrl/admin/upstreams'), headers: _headers);
    _ensureOk(r);
    return (jsonDecode(r.body) as List).map((e) => Upstream.fromJson(e)).toList();
  }

  Future<List<ModelEntry>> models() async {
    final r = await _http.get(Uri.parse('$baseUrl/admin/models'), headers: _headers);
    _ensureOk(r);
    return (jsonDecode(r.body) as List).map((e) => ModelEntry.fromJson(e)).toList();
  }

  Future<List<GatewayEvent>> events({int since = 0}) async {
    final r = await _http.get(Uri.parse('$baseUrl/admin/events?since=$since'), headers: _headers);
    _ensureOk(r);
    final list = jsonDecode(r.body);
    if (list == null) return [];
    return (list as List).map((e) => GatewayEvent.fromJson(e)).toList();
  }

  Future<Map<String, dynamic>> summary() async {
    final r = await _http.get(Uri.parse('$baseUrl/admin/summary'), headers: _headers);
    _ensureOk(r);
    return jsonDecode(r.body);
  }

  Future<void> setEnabled(String name, bool enabled) async {
    final r = await _http.post(
      Uri.parse('$baseUrl/admin/upstreams/$name/${enabled ? 'enable' : 'disable'}'),
      headers: _headers,
    );
    _ensureOk(r);
  }

  Future<List<dynamic>> probe(String name) async {
    final r = await _http.post(Uri.parse('$baseUrl/admin/upstreams/$name/probe'), headers: _headers);
    _ensureOk(r);
    return jsonDecode(r.body) as List;
  }

  void _ensureOk(http.Response r) {
    if (r.statusCode >= 400) {
      throw ApiException(r.statusCode, r.body);
    }
  }
}

class ApiException implements Exception {
  final int status;
  final String body;
  ApiException(this.status, this.body);
  @override
  String toString() => 'HTTP $status: $body';
}
