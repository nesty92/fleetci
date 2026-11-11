# gson: mvn verify, as its CI, with the local repository on the cache disk.
M="$RUN maven:3-eclipse-temurin-21 mvn -B -ntp -Dmaven.repo.local=/cache/m2"
pull maven:3-eclipse-temurin-21
step verify "$M verify javadoc:jar"
