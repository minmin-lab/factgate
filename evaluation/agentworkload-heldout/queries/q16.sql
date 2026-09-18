SELECT department, SUM(total_amount) AS total_amount, SUM(request_count) AS request_count FROM expense_summary WHERE month = '2026-03' GROUP BY department
