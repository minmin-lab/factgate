SELECT city, COUNT(DISTINCT employee_no) AS employee_count FROM expense_detail GROUP BY city;
