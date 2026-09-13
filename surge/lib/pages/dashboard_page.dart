import 'dart:async';

import 'package:flutter/material.dart';

import '../api/client.dart';
import '../theme/tokens.dart';

// Dashboard follows KiroaaS App.tsx status card: whole card turns lime when
// healthy, big status word, summary numbers.
class DashboardPage extends StatefulWidget {
  final ApiClient api;
  const DashboardPage({super.key, required this.api});

  @override
  State<DashboardPage> createState() => _DashboardPageState();
}

class _DashboardPageState extends State<DashboardPage> {
  Map<String, dynamic>? summary;
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
      final s = await widget.api.summary();
      if (mounted) setState(() { summary = s; error = null; });
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
    final s = summary;
    if (s == null) {
      if (error != null) {
        return Center(
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              const Icon(Icons.cloud_off_outlined, size: 40, color: Tokens.textMuted),
              const SizedBox(height: 16),
              Text('Cannot reach admin API: $error',
                  style: const TextStyle(color: Tokens.err), textAlign: TextAlign.center),
              const SizedBox(height: 8),
              const Text('Check address & token in Settings',
                  style: TextStyle(color: Tokens.textMuted)),
            ],
          ),
        );
      }
      return const Center(child: CircularProgressIndicator());
    }
    final byStatus = (s['by_status'] as Map?)?.cast<String, dynamic>() ?? {};
    final enabled = byStatus['enabled'] ?? 0;
    final total = s['total'] ?? 0;
    final healthy = enabled == total && (total as int) > 0;
    final balance = (s['balance_usd'] as num?)?.toDouble() ?? 0;

    return ListView(
      padding: const EdgeInsets.all(24),
      children: [
        AnimatedContainer(
          duration: const Duration(milliseconds: 300),
          padding: const EdgeInsets.all(32),
          decoration: BoxDecoration(
            color: healthy ? Tokens.accent : Tokens.cardBg,
            borderRadius: BorderRadius.circular(Tokens.cardRadius),
          ),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text('STATUS', style: Theme.of(context).textTheme.labelSmall),
              const SizedBox(height: 8),
              Text(healthy ? 'All systems go' : '$enabled / $total online',
                  style: Theme.of(context).textTheme.displaySmall),
              const SizedBox(height: 16),
              Text(
                'enabled ${byStatus['enabled'] ?? 0} · '
                'auto-disabled ${byStatus['auto_disabled'] ?? 0} · '
                'manual ${byStatus['manually_disabled'] ?? 0} · '
                'half-open ${byStatus['half_open'] ?? 0}',
                style: const TextStyle(color: Tokens.textMuted),
              ),
            ],
          ),
        ),
        const SizedBox(height: 16),
        Container(
          padding: const EdgeInsets.all(32),
          decoration: BoxDecoration(
            color: Tokens.cardBg,
            borderRadius: BorderRadius.circular(Tokens.cardRadius),
          ),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text('TOTAL BALANCE', style: Theme.of(context).textTheme.labelSmall),
              const SizedBox(height: 8),
              Text('\$${balance.toStringAsFixed(2)}',
                  style: Theme.of(context).textTheme.displaySmall),
              Text('${s['balance_known'] ?? 0} upstreams reporting',
                  style: const TextStyle(color: Tokens.textMuted)),
            ],
          ),
        ),
      ],
    );
  }
}
