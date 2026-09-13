import 'dart:async';

import 'package:flutter/material.dart';

import '../api/client.dart';
import '../theme/tokens.dart';

class EventsPage extends StatefulWidget {
  final ApiClient api;
  const EventsPage({super.key, required this.api});

  @override
  State<EventsPage> createState() => _EventsPageState();
}

class _EventsPageState extends State<EventsPage> {
  final List<GatewayEvent> events = [];
  int lastSeq = 0;
  String? error;
  Timer? timer;

  @override
  void initState() {
    super.initState();
    _load();
    timer = Timer.periodic(const Duration(seconds: 2), (_) => _load());
  }

  Future<void> _load() async {
    try {
      final batch = await widget.api.events(since: lastSeq);
      if (mounted) {
        setState(() {
          error = null;
          if (batch.isNotEmpty) {
            events.addAll(batch);
            lastSeq = batch.last.seq;
            if (events.length > 500) events.removeRange(0, events.length - 500);
          }
        });
      }
    } catch (e) {
      if (mounted) setState(() => error = e.toString());
    }
  }

  @override
  void dispose() {
    timer?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    if (events.isEmpty && error != null) {
      return Center(
        child: Text('Cannot reach admin API: $error',
            style: const TextStyle(color: Tokens.err), textAlign: TextAlign.center),
      );
    }
    final reversed = events.reversed.toList();
    return Container(
      margin: const EdgeInsets.all(24),
      padding: const EdgeInsets.all(20),
      decoration: BoxDecoration(
        color: Tokens.logBg,
        borderRadius: BorderRadius.circular(Tokens.logRadius),
      ),
      child: Column(
        children: [
          if (error != null)
            Padding(
              padding: const EdgeInsets.only(bottom: 8),
              child: Text('connection lost: $error',
                  style: const TextStyle(color: Tokens.err, fontSize: 12)),
            ),
          Expanded(
            child: ListView.builder(
              itemCount: reversed.length,
              itemBuilder: (_, i) {
                final e = reversed[i];
                return Padding(
                  padding: const EdgeInsets.symmetric(vertical: 2),
                  child: Text(
                    '[${e.at}] ${e.upstream}: ${e.from} → ${e.to}  ${e.reason}',
                    style: const TextStyle(
                        fontFamily: 'monospace', fontSize: 12, color: Color(0xFFD4D4D4)),
                  ),
                );
              },
            ),
          ),
        ],
      ),
    );
  }
}
