import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import '../api/client.dart';
import '../theme/tokens.dart';

class ModelsPage extends StatefulWidget {
  final ApiClient api;
  const ModelsPage({super.key, required this.api});

  @override
  State<ModelsPage> createState() => _ModelsPageState();
}

class _ModelsPageState extends State<ModelsPage> {
  List<ModelEntry> items = [];
  String? error;
  Timer? timer;

  @override
  void initState() {
    super.initState();
    _load();
    timer = Timer.periodic(const Duration(minutes: 2), (_) => _load());
  }

  Future<void> _load() async {
    try {
      final list = await widget.api.models();
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
    if (items.isEmpty && error != null) {
      return Center(
        child: Text('Cannot reach admin API: $error',
            style: const TextStyle(color: Tokens.err), textAlign: TextAlign.center),
      );
    }
    return ListView.separated(
      padding: const EdgeInsets.all(24),
      itemCount: items.length,
      separatorBuilder: (_, __) => const SizedBox(height: 12),
      itemBuilder: (_, i) {
        final m = items[i];
        return Container(
          padding: const EdgeInsets.symmetric(horizontal: 24, vertical: 16),
          decoration: BoxDecoration(
            color: Tokens.cardBg,
            borderRadius: BorderRadius.circular(Tokens.navRadius),
          ),
          child: Row(
            children: [
              Expanded(
                child: Text(m.name,
                    style: const TextStyle(fontFamily: 'monospace', fontWeight: FontWeight.bold)),
              ),
              Text('${m.available}/${m.upstreams} upstreams',
                  style: const TextStyle(color: Tokens.textMuted, fontSize: 12)),
              IconButton(
                icon: const Icon(Icons.copy, size: 16),
                tooltip: 'Copy model name',
                onPressed: () => Clipboard.setData(ClipboardData(text: m.name)),
              ),
            ],
          ),
        );
      },
    );
  }
}
