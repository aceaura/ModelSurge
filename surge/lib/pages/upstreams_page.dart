import 'dart:async';

import 'package:flutter/material.dart';

import '../api/client.dart';
import '../widgets/upstream_card.dart';

class UpstreamsPage extends StatefulWidget {
  final ApiClient api;
  const UpstreamsPage({super.key, required this.api});

  @override
  State<UpstreamsPage> createState() => _UpstreamsPageState();
}

class _UpstreamsPageState extends State<UpstreamsPage> {
  List<Upstream> items = [];
  final Map<String, double> maxBalanceSeen = {};
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
      final list = await widget.api.upstreams();
      for (final u in list) {
        final b = u.lastBalance;
        if (b != null && b > (maxBalanceSeen[u.name] ?? 0)) {
          maxBalanceSeen[u.name] = b;
        }
      }
      if (mounted) setState(() { items = list; error = null; });
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
    if (error != null && items.isEmpty) {
      return Center(child: Text('Failed to load: $error'));
    }
    return ListView.separated(
      padding: const EdgeInsets.all(24),
      itemCount: items.length,
      separatorBuilder: (_, __) => const SizedBox(height: 16),
      itemBuilder: (_, i) {
        final u = items[i];
        return UpstreamCard(
          upstream: u,
          maxBalance: maxBalanceSeen[u.name],
          onToggle: (enabled) async {
            await widget.api.setEnabled(u.name, enabled);
            await _load();
          },
          onProbe: () async {
            final result = await widget.api.probe(u.name);
            await _load();
            return result;
          },
        );
      },
    );
  }
}
